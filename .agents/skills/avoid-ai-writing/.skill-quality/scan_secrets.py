"""Run checksum-pinned Gitleaks locally; never upload repository content."""
import argparse
import hashlib
import io
import platform
import subprocess
import tarfile
import tempfile
import urllib.request
from pathlib import Path

VERSION = "8.30.1"
HASHES = {
    ("Darwin", "arm64"): "b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5",
    ("Darwin", "x86_64"): "dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709",
    ("Linux", "aarch64"): "e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080",
    ("Linux", "x86_64"): "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb",
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("repository", nargs="?", default=".")
    parser.add_argument("--output-directory", required=True)
    args = parser.parse_args()
    root = Path(args.repository).resolve()
    output = Path(args.output_directory).resolve()
    if output.is_relative_to(root):
        raise SystemExit("write redacted reports outside the scanned repository")
    output.mkdir(parents=True, exist_ok=True)
    system, machine = platform.system(), platform.machine()
    expected = HASHES.get((system, machine))
    if not expected:
        raise SystemExit(f"unsupported platform: {system}/{machine}")
    arch = "arm64" if machine in ("arm64", "aarch64") else "x64"
    asset = f"gitleaks_{VERSION}_{system.lower()}_{arch}.tar.gz"
    url = f"https://github.com/gitleaks/gitleaks/releases/download/v{VERSION}/{asset}"
    with tempfile.TemporaryDirectory(prefix="skill-quality-gitleaks-") as directory:
        with urllib.request.urlopen(url, timeout=60) as response:
            archive = response.read()
        if hashlib.sha256(archive).hexdigest() != expected:
            raise SystemExit("Gitleaks archive checksum mismatch")
        binary = Path(directory) / "gitleaks"
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as tar:
            entry = tar.getmember("gitleaks")
            if not entry.isfile():
                raise SystemExit("Gitleaks archive entry is not a file")
            binary.write_bytes(tar.extractfile(entry).read())
        binary.chmod(0o700)
        codes = []
        for mode in ("git", "dir"):
            # A dedicated findings code distinguishes a detected secret from tool failure.
            command = [str(binary), mode, str(root), "--redact=100", "--no-banner", "--exit-code", "10",
                       "--report-format", "json", "--report-path", str(output / f"gitleaks-{mode}.json")]
            if mode == "git":
                command += ["--log-opts=--all"]
            result = subprocess.run(command, cwd=root, timeout=600)
            codes.append(result.returncode)
        if any(c not in (0, 10) for c in codes):
            raise SystemExit("Gitleaks did not complete successfully")
        return 1 if 10 in codes else 0


if __name__ == "__main__":
    raise SystemExit(main())
