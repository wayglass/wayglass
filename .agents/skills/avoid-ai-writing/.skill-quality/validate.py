"""Validate an explicitly inventoried skill repository without executing its code."""
import argparse
import hashlib
import json
import os
import re
import subprocess
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import unquote, urlsplit

import yaml
from jsonschema import Draft202012Validator


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader, node, deep=False):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in result:
            raise ValueError(f"duplicate YAML key: {key}")
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping)


def within(root, path):
    return path.resolve().is_relative_to(root.resolve())


def frontmatter(path):
    text = path.read_text(encoding="utf-8")
    match = re.match(r"\A---\r?\n(.*?)\r?\n---(?:\r?\n|$)(.*)\Z", text, re.S)
    if not match:
        raise ValueError("missing or unterminated YAML frontmatter")
    data = yaml.load(match[1], Loader=UniqueLoader)
    if not isinstance(data, dict):
        raise ValueError("frontmatter must be an object")
    name = data.get("name")
    if not isinstance(name, str) or not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", name) or len(name) > 64:
        raise ValueError("name must be 1-64 lowercase letters, digits, and single hyphens")
    description = data.get("description")
    if not isinstance(description, str) or not description.strip() or len(description) > 1024:
        raise ValueError("description must be a nonempty string of at most 1024 characters")
    return data, match[2]


def validate(root):
    root = Path(root).resolve()
    findings = []
    skills = []
    def issue(code, path, message, severity="error"):
        findings.append(dict(code=code, path=str(path), message=message, severity=severity))
    try:
        config = json.loads((root / ".skill-quality.json").read_text())
        if not isinstance(config, dict):
            raise ValueError("configuration must be an object")
        paths = config["skills"]
        if not isinstance(paths, list) or not paths or any(not isinstance(p, str) for p in paths):
            raise ValueError("skills must be a nonempty array of paths")
        if len(paths) != len(set(paths)):
            raise ValueError("duplicate skill paths")
        for check in config.get("checks", []):
            if not isinstance(check.get("name"), str) or not check["name"]:
                raise ValueError("each check needs a name")
            command = check.get("command")
            if not isinstance(command, list) or not command or any(not isinstance(s, str) for s in command):
                raise ValueError("check commands must be nonempty argument arrays")
            if not within(root, root / check.get("cwd", ".")):
                raise ValueError("check cwd escapes the repository")
    except (OSError, ValueError, KeyError, TypeError) as exc:
        issue("configuration", ".skill-quality.json", str(exc))
        paths = []
    for relative in paths:
        path = root / relative
        if Path(relative).is_absolute() or not within(root, path):
            issue("path-escape", relative, "skill path escapes the repository")
            continue
        try:
            data, body = frontmatter(path)
            skills.append(dict(name=data["name"], path=relative, sha256=hashlib.sha256(path.read_bytes()).hexdigest()))
            # Links in examples are data, not installation dependencies.
            prose = re.sub(r"(?ms)^(`{3,}|~{3,}).*?^\1[^\n]*$", "", body)
            for target in re.findall(r"(?<!!)\[[^\]\n]*\]\(([^\s)]+)(?:\s+\"[^\"]*\")?\)", prose):
                target = target.strip("<>")
                parsed = urlsplit(target)
                if parsed.scheme or parsed.netloc or not parsed.path or parsed.path.startswith("/"):
                    continue
                linked = path.parent / unquote(parsed.path)
                if not within(root, linked):
                    issue("link-escape", relative, f"relative link leaves repository: {target}")
                elif not linked.exists():
                    issue("missing-link", relative, f"missing relative file: {target}")
            if len(body.splitlines()) > 500:
                issue("entrypoint-length", relative, "entrypoint exceeds 500 lines; review progressive disclosure", "warning")
        except (OSError, ValueError, yaml.YAMLError) as exc:
            issue("frontmatter", relative, str(exc))
    try:
        page = json.loads((root / "skills.sh.json").read_text())
        schema = json.loads((Path(__file__).parent / "skills.sh.schema.json").read_text())
        schema_errors = list(Draft202012Validator(schema).iter_errors(page))
        for error in schema_errors:
            issue("page-schema", "skills.sh.json", error.message)
        grouped = [] if schema_errors else [n for g in page["groupings"] for n in g["skills"]]
        names = {s["name"] for s in skills}
        if len(grouped) != len(set(grouped)):
            issue("duplicate-grouping", "skills.sh.json", "skill occurs in multiple groups")
        for name in set(grouped) - names:
            issue("unknown-grouping", "skills.sh.json", f"grouped skill is absent from inventory: {name}")
        for name in names - set(grouped):
            issue("ungrouped", "skills.sh.json", f"skill is not explicitly grouped: {name}", "warning")
    except (OSError, ValueError, TypeError, KeyError) as exc:
        issue("page-schema", "skills.sh.json", str(exc))
    try:
        tracked = subprocess.check_output(["git", "-C", str(root), "ls-files", "-z"], text=True).split("\0")
        for relative in filter(None, tracked):
            path = root / relative
            if path.is_symlink() and (not within(root, path) or not path.exists()):
                issue("unsafe-symlink", relative, "tracked symlink is broken or leaves repository")
            if "__pycache__" in path.parts or path.suffix == ".pyc" or path.name == ".DS_Store":
                issue("generated-file", relative, "generated local file is tracked")
        revision = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True, stderr=subprocess.DEVNULL).strip()
        dirty = bool(subprocess.check_output(["git", "-C", str(root), "status", "--porcelain"], text=True))
    except subprocess.CalledProcessError:
        revision, dirty = None, True
    digest = hashlib.sha256()
    candidates = subprocess.check_output(["git", "-C", str(root), "ls-files", "--cached", "--others", "--exclude-standard", "-z"], text=True).split("\0")
    for relative in sorted(set(filter(None, candidates))):
        path = root / relative
        digest.update(relative.encode() + b"\0")
        if path.is_symlink():
            digest.update(b"symlink\0" + os.readlink(path).encode())
        elif path.is_file():
            digest.update(path.read_bytes())
        else:
            digest.update(b"missing")
        digest.update(b"\0")
    return dict(version=1, checkedAt=datetime.now(timezone.utc).isoformat(), revision=revision,
                contentHash=digest.hexdigest(), validatorHash=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                dirty=dirty, status="fail" if any(f["severity"] == "error" for f in findings) else "pass",
                scope="declared entrypoints, page config, tracked file hygiene", skills=skills, findings=findings,
                notAssessed=["semantic security", "behavioral quality", "provider audits", "redistribution rights", "installer compatibility"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("repository", nargs="?", default=".")
    parser.add_argument("--output")
    args = parser.parse_args()
    report = validate(args.repository)
    if args.output:
        Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
    for finding in report["findings"]:
        print(f"{finding['severity']}: {finding['path']}: {finding['message']}")
    print(f"Skill validation: {report['status']}")
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
