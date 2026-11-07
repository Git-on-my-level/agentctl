#!/usr/bin/env python3
"""Fail when a release removes a config key or value that host bundles may still carry.

Host bundles are private and cannot be read from this repository, so this check
covers the part that can be decided here: every key path and enum value that the
config-bundle schema accepted at the previous release tag must still be accepted,
or be recorded in docs/config-key-removals.json. A recorded removal tells the
operator which key to delete from bundles before the release reaches a host, and
agentctl-config's bundle check (run against that release) confirms the cleanup.

Usage: scripts/check-config-removals.py [--root DIR] [--base REV] [--head REV]
  --base defaults to the newest v* tag that is an ancestor of HEAD and does not
  point at it. With no earlier tag there is nothing to compare and the check passes.
  Without --head the working-tree schema is checked, so an uncommitted removal fails
  locally; --head REV checks a committed revision, as release tags do.
  A property that gains an enum (or const) where it accepted any value is reported
  as "PATH!restricted": bundles holding other values stop validating.
  The ledger is a permanent record: entries for removals in older releases stay.
Exit 0 when every removal is recorded, 1 otherwise. Prints one JSON summary line.
"""
import argparse
import json
import pathlib
import subprocess
import sys

SCHEMA = "schemas/config-bundle.schema.json"
LEDGER = "docs/config-key-removals.json"


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True, text=True).stdout.strip()


def previous_release(root, head):
    head_commit = git(root, "rev-parse", f"{head}^{{commit}}")
    for tag in git(root, "tag", "--merged", head, "--list", "v*", "--sort=-v:refname").splitlines():
        if git(root, "rev-parse", f"{tag}^{{commit}}") != head_commit:
            return tag
    return None


def walk_schema(schema):
    """Return (key paths and enum values, restricted paths, all node paths).

    $ref targets are expanded inline at each use site, so a key is reported at the
    exact location a host bundle would write it. Recursive definitions stop at the
    first repeat on the current path. A path is restricted when its node carries an
    enum or const; nodes lists every property, item, and map-value path seen.
    """
    defs = schema.get("$defs", {})
    found = set()
    restricted = set()
    nodes = set()

    def resolve(node, stack):
        ref = node.get("$ref")
        if isinstance(ref, str) and ref.startswith("#/$defs/"):
            name = ref[len("#/$defs/"):]
            if name in stack:
                return None, stack
            return defs.get(name, {}), stack + (name,)
        return node, stack

    def walk(node, path, stack):
        if not isinstance(node, dict):
            return
        node, stack = resolve(node, stack)
        if node is None:
            return
        values = list(node.get("enum", []))
        if "const" in node:
            values.append(node["const"])
        if path:
            nodes.add(path)
        if values and path:
            restricted.add(path)
        for value in values:
            found.add(f"{path}={json.dumps(value)}")
        for name, child in (node.get("properties") or {}).items():
            child_path = f"{path}.{name}" if path else name
            found.add(child_path)
            walk(child, child_path, stack)
        if isinstance(node.get("additionalProperties"), dict):
            walk(node["additionalProperties"], f"{path}.*" if path else "*", stack)
        if isinstance(node.get("items"), dict):
            walk(node["items"], f"{path}[]", stack)
        for combinator in ("allOf", "anyOf", "oneOf"):
            for branch in node.get(combinator, []):
                walk(branch, path, stack)

    walk(schema, "", ())
    return found, restricted, nodes


def key_paths(schema):
    """Return every config key path and enum value the schema accepts."""
    return walk_schema(schema)[0]


def covering_names(key):
    """Ledger names that record a removal: the key itself, or for an enum value or a
    restriction the path it belongs to. A parent object is not named, so a ledger
    entry for one field never silently covers a different removal inside it.
    """
    if key.endswith("!restricted"):
        return [key, key[: -len("!restricted")]]
    return [key, key.split("=", 1)[0]]


def load_schema_at(root, rev):
    """Schema at a git revision, or the working-tree file when rev is None."""
    if rev is None:
        return json.loads((root / SCHEMA).read_text())
    return json.loads(git(root, "show", f"{rev}:{SCHEMA}"))


def load_ledger(root):
    path = root / LEDGER
    if not path.is_file():
        return {}, None
    try:
        doc = json.loads(path.read_text())
    except json.JSONDecodeError as exc:
        return {}, f"{LEDGER}: not JSON: {exc}"
    entries = {}
    for entry in doc.get("removed") or []:
        key = entry.get("key")
        if isinstance(key, str) and key:
            entries[key] = entry
    return entries, None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=pathlib.Path(__file__).resolve().parent.parent, type=pathlib.Path)
    ap.add_argument("--base", default=None)
    ap.add_argument("--head", default=None)
    args = ap.parse_args()
    root = args.root.resolve()

    base = args.base or previous_release(root, args.head or "HEAD")
    if base is None:
        print(json.dumps({"ok": True, "base": None, "removed": [], "errors": [], "note": "no earlier release tag to compare"}))
        return 0

    before_keys, before_restricted, before_nodes = walk_schema(load_schema_at(root, base))
    after_keys, after_restricted, _ = walk_schema(load_schema_at(root, args.head))
    removed = sorted(before_keys - after_keys)
    # A node that already existed and newly gains an enum now rejects values it used to accept.
    removed += sorted(f"{path}!restricted" for path in (after_restricted - before_restricted) if path in before_nodes)
    ledger, ledger_error = load_ledger(root)
    errors = [ledger_error] if ledger_error else []
    for key in removed:
        covering = [ledger[name] for name in covering_names(key) if name in ledger]
        if not covering:
            errors.append(
                f"{key}: removed since {base} but not recorded in {LEDGER}; "
                "add an entry naming the release and what operators must delete from bundles"
            )
            continue
        entry = covering[0]
        if not str(entry.get("removed_in", "")).strip() or not str(entry.get("note", "")).strip():
            errors.append(f"{key}: ledger entry needs removed_in and note")
    print(json.dumps({"ok": not errors, "base": base, "removed": removed, "errors": errors}))
    return 0 if not errors else 1


if __name__ == "__main__":
    sys.exit(main())
