"""Run the repository's explicitly configured checks, without a shell."""
import argparse
import json
import subprocess
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("repository", nargs="?", default=".")
    args = parser.parse_args()
    root = Path(args.repository).resolve()
    config = json.loads((root / ".skill-quality.json").read_text())
    checks = config.get("checks", [])
    if not checks:
        print("No repository-specific executable tests declared; behavioral evaluation remains untested.")
    for check in checks:
        cwd = (root / check.get("cwd", ".")).resolve()
        if not cwd.is_relative_to(root):
            raise ValueError("check working directory leaves repository")
        print(f"Running {check['name']}", flush=True)
        subprocess.run(check["command"], cwd=cwd, check=True, timeout=600)


if __name__ == "__main__":
    main()
