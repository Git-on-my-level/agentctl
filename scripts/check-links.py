#!/usr/bin/env python3
"""Check repository-relative Markdown links without making network requests."""

from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote


ROOT = Path(__file__).resolve().parents[1]
MARKDOWN_LINK = re.compile(r"!?(?:\[[^\]]*\])\(([^)\s]+)(?:\s+[^)]*)?\)")
INLINE_ANCHOR = re.compile(r"<a\s+[^>]*id=[\"']([^\"']+)[\"']", re.IGNORECASE)
HEADING = re.compile(r"^#{1,6}\s+(.+?)\s*#*\s*$")


def slug(text: str) -> str:
    text = re.sub(r"[`*_~]", "", text).lower()
    text = re.sub(r"[^a-z0-9 -]", "", text)
    return re.sub(r"[- ]+", "-", text).strip("-")


def anchors(path: Path) -> set[str]:
    values: set[str] = set()
    for line in path.read_text(encoding="utf-8").splitlines():
        match = HEADING.match(line)
        if match:
            values.add(slug(match.group(1)))
        for anchor in INLINE_ANCHOR.finditer(line):
            values.add(anchor.group(1))
    return values


def main() -> int:
    failures: list[str] = []
    for source in sorted([ROOT / "README.md", *((ROOT / "docs").rglob("*.md"))]):
        if not source.is_file():
            continue
        content = source.read_text(encoding="utf-8")
        # Links in fenced examples are still useful to check, but links to
        # commands/URLs are skipped by the target classification below.
        for match in MARKDOWN_LINK.finditer(content):
            target = unquote(match.group(1)).strip("<>")
            if not target or target.startswith(("#", "/", "http://", "https://", "mailto:", "data:")):
                continue
            path_part, separator, fragment = target.partition("#")
            resolved = (source.parent / path_part).resolve()
            if ROOT not in resolved.parents and resolved != ROOT:
                failures.append(f"{source.relative_to(ROOT)}: link escapes repository: {target}")
                continue
            if not resolved.exists():
                failures.append(f"{source.relative_to(ROOT)}: missing link target: {target}")
                continue
            if separator and fragment and resolved.is_file() and resolved.suffix.lower() == ".md":
                if fragment not in anchors(resolved):
                    failures.append(f"{source.relative_to(ROOT)}: missing anchor: {target}")

    if failures:
        for failure in failures:
            print(failure, file=sys.stderr)
        return 1
    print("link check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
