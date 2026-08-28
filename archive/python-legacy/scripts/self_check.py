from __future__ import annotations

import compileall
import re
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SKIP = {".git", ".local", ".runtime", "__pycache__", "dist", "build"}
TEXT_SUFFIXES = {".cfg", ".cmd", ".ini", ".json", ".md", ".ps1", ".py", ".sh", ".toml", ".txt", ".yaml", ".yml"}
TEXT_NAMES = {".gitattributes", ".gitignore", "LICENSE"}
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
SECRET_PATTERNS = {
    "GitHub token": re.compile(r"github_pat_[A-Za-z0-9_]{20,}"),
    "GitHub classic token": re.compile(r"gh[pousr]_[A-Za-z0-9]{20,}"),
    "OpenAI-style key": re.compile(r"sk-[A-Za-z0-9_-]{20,}"),
    "private key": re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
}


def scan_secrets() -> list[str]:
    findings: list[str] = []
    for path in ROOT.rglob("*"):
        if not path.is_file() or any(part in SKIP for part in path.parts):
            continue
        if path.suffix.lower() not in TEXT_SUFFIXES and path.name not in TEXT_NAMES:
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeError:
            continue
        for label, pattern in SECRET_PATTERNS.items():
            if pattern.search(text):
                findings.append(f"{path.relative_to(ROOT)}: {label}")
    return findings


def main() -> int:
    print("[1/3] compile")
    if not compileall.compile_dir(ROOT / "codexpc_connector", quiet=1):
        return 1
    if not compileall.compile_file(ROOT / "main.py", quiet=1):
        return 1

    print("[2/3] unit tests")
    suite = unittest.defaultTestLoader.discover(str(ROOT / "tests"))
    result = unittest.TextTestRunner(verbosity=1).run(suite)
    if not result.wasSuccessful():
        return 1

    print("[3/3] secret scan")
    findings = scan_secrets()
    if findings:
        print("Potential secrets found:")
        for finding in findings:
            print(f"- {finding}")
        return 1

    check = subprocess.run(
        ["git", "diff", "--check"],
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=False,
    )
    if check.returncode not in {0, 129}:
        print(check.stdout)
        print(check.stderr)
        return 1

    print("Self-check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
