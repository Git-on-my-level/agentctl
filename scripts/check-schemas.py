#!/usr/bin/env python3
"""Check schema JSON syntax, local references, and documentation pointers."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
SCHEMA_DIR = ROOT / "schemas"


def pointer_get(document: Any, pointer: str) -> Any:
    if pointer == "":
        return document
    current = document
    for raw_part in pointer.lstrip("/").split("/"):
        part = raw_part.replace("~1", "/").replace("~0", "~")
        if isinstance(current, dict):
            if part not in current:
                raise KeyError(part)
            current = current[part]
        elif isinstance(current, list):
            current = current[int(part)]
        else:
            raise KeyError(part)
    return current


def walk_refs(value: Any):
    if isinstance(value, dict):
        ref = value.get("$ref")
        if isinstance(ref, str):
            yield ref
        for child in value.values():
            yield from walk_refs(child)
    elif isinstance(value, list):
        for child in value:
            yield from walk_refs(child)


def main() -> int:
    if not SCHEMA_DIR.is_dir():
        print(f"schema directory not found: {SCHEMA_DIR}", file=sys.stderr)
        return 1

    files = sorted(SCHEMA_DIR.glob("*.json"))
    if not files:
        print("no schema files found", file=sys.stderr)
        return 1

    ids: dict[str, Path] = {}
    failures: list[str] = []
    for path in files:
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            failures.append(f"{path.relative_to(ROOT)}: invalid JSON ({exc})")
            continue
        if not isinstance(document, dict):
            failures.append(f"{path.relative_to(ROOT)}: root must be an object")
            continue

        schema_id = document.get("$id")
        if not isinstance(schema_id, str) or not schema_id:
            failures.append(f"{path.relative_to(ROOT)}: missing non-empty $id")
        elif schema_id in ids:
            failures.append(
                f"{path.relative_to(ROOT)}: duplicate $id also used by {ids[schema_id].relative_to(ROOT)}"
            )
        else:
            ids[schema_id] = path

        docs_pointer = document.get("x-agentctl-docs")
        if docs_pointer is not None:
            if not isinstance(docs_pointer, str) or not docs_pointer:
                failures.append(f"{path.relative_to(ROOT)}: x-agentctl-docs must be a string")
            else:
                docs_path = docs_pointer.split("#", 1)[0]
                target = (path.parent / docs_path).resolve()
                if not target.is_file() or ROOT not in target.parents:
                    failures.append(
                        f"{path.relative_to(ROOT)}: documentation target does not exist: {docs_pointer}"
                    )

        for ref in walk_refs(document):
            if ref.startswith("#/"):
                try:
                    pointer_get(document, ref[1:])
                except (KeyError, ValueError, IndexError):
                    failures.append(f"{path.relative_to(ROOT)}: unresolved local $ref {ref}")

    if failures:
        for failure in failures:
            print(failure, file=sys.stderr)
        return 1
    print(f"schema check passed ({len(files)} files)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
