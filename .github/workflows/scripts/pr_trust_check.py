#!/usr/bin/env python3
"""Shared trust boundary for pull_request_target workflows.

A PR author is trusted when their author_association with this repository is
OWNER, MEMBER, or COLLABORATOR, or when their login appears in
.github/blacksmith-allowlist.txt. Reads PR_AUTHOR and PR_ASSOCIATION from the
environment so callers never have to duplicate this policy inline. Exits 0
when trusted, 1 when not.
"""
import os
import sys
from pathlib import Path

TRUSTED_ASSOCIATIONS = {"OWNER", "MEMBER", "COLLABORATOR"}
ALLOWLIST_PATH = Path(".github/blacksmith-allowlist.txt")


def load_allowlist(path: Path) -> set[str]:
    if not path.exists():
        return set()
    allowlist = set()
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.split("#", 1)[0].strip()
        if line:
            allowlist.add(line.lower())
    return allowlist


def is_trusted(author: str, association: str, allowlist: set[str]) -> bool:
    normalized_author = author.strip().lower()
    normalized_association = association.strip().upper()
    return normalized_association in TRUSTED_ASSOCIATIONS or normalized_author in allowlist


def main() -> int:
    author = os.environ.get("PR_AUTHOR", "")
    association = os.environ.get("PR_ASSOCIATION", "")
    allowlist = load_allowlist(ALLOWLIST_PATH)
    trusted = is_trusted(author, association, allowlist)
    print(
        f"author={author or '<unknown>'} association={association or '<unknown>'} "
        f"trusted={str(trusted).lower()}"
    )
    return 0 if trusted else 1


if __name__ == "__main__":
    sys.exit(main())
