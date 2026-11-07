# Checks scripts/check-config-removals.py: key-path extraction, ledger coverage,
# and end-to-end behaviour against a throwaway repository with release tags.
import importlib.util
import json
import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("check_config_removals", ROOT / "scripts" / "check-config-removals.py")
checker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(checker)


def schema(mode_values):
    pool = {
        "type": "object",
        "properties": {"candidates": {"type": "array"}},
    }
    if mode_values is not None:
        pool["properties"]["mode"] = {"enum": mode_values}
    return {
        "$defs": {"capacity_pool": pool},
        "type": "object",
        "properties": {
            "profiles": {
                "type": "object",
                "additionalProperties": {
                    "type": "object",
                    "properties": {
                        "capacity": {
                            "type": "object",
                            "properties": {
                                "pools": {"type": "object", "additionalProperties": {"$ref": "#/$defs/capacity_pool"}}
                            },
                        }
                    },
                },
            }
        },
    }


class KeyPathTests(unittest.TestCase):
    def test_refs_expand_at_use_site_and_enum_values_are_recorded(self):
        keys = checker.key_paths(schema(["priority", "balance"]))
        self.assertIn("profiles.*.capacity.pools.*.mode", keys)
        self.assertIn('profiles.*.capacity.pools.*.mode="priority"', keys)
        self.assertIn("profiles.*.capacity.pools.*.candidates", keys)

    def test_removed_property_is_missing_from_later_schema(self):
        before = checker.key_paths(schema(["priority"]))
        after = checker.key_paths(schema(None))
        self.assertEqual(
            sorted(before - after),
            ['profiles.*.capacity.pools.*.mode', 'profiles.*.capacity.pools.*.mode="priority"'],
        )

    def test_free_form_property_becoming_enum_is_restricted(self):
        free = {"type": "object", "properties": {"executable": {"type": "string"}}}
        narrowed = {"type": "object", "properties": {"executable": {"type": "string", "enum": ["/usr/bin/codex"]}}}
        before_keys, before_restricted, before_nodes = checker.walk_schema(free)
        after_keys, after_restricted, _ = checker.walk_schema(narrowed)
        self.assertEqual(before_keys - after_keys, set())
        self.assertIn("executable", before_nodes)
        self.assertEqual(sorted(p for p in after_restricted - before_restricted if p in before_nodes), ["executable"])
        self.assertIn("executable", checker.covering_names("executable!restricted"))

    def test_recursive_definitions_terminate(self):
        loop = {"$defs": {"node": {"type": "object", "properties": {"child": {"$ref": "#/$defs/node"}}}},
                "type": "object", "properties": {"root": {"$ref": "#/$defs/node"}}}
        self.assertIn("root.child", checker.key_paths(loop))

    def test_ledger_entry_covers_enum_values_beneath_it(self):
        self.assertIn("profiles.*.capacity.pools.*.mode", checker.covering_names('profiles.*.capacity.pools.*.mode="balance"'))
        self.assertNotIn("profiles.*.capacity.pools.*", checker.covering_names('profiles.*.capacity.pools.*.mode'))


def git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True,
                   env={"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
                        "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
                        "PATH": __import__("os").environ["PATH"], "GIT_CONFIG_NOSYSTEM": "1",
                        "HOME": cwd})


class EndToEndTests(unittest.TestCase):
    def make_repo(self, root, schemas_by_tag, ledger=None):
        git(root, "init", "--quiet")
        (root / "schemas").mkdir()
        if ledger is not None:
            (root / "docs").mkdir()
        for tag, doc in schemas_by_tag:
            (root / "schemas" / "config-bundle.schema.json").write_text(json.dumps(doc))
            if ledger is not None:
                (root / "docs" / "config-key-removals.json").write_text(json.dumps(ledger))
            git(root, "add", "-A")
            git(root, "commit", "--quiet", "--allow-empty", "-m", tag)
            git(root, "tag", tag)

    def run_check(self, root):
        return subprocess.run(["python3", str(ROOT / "scripts" / "check-config-removals.py"), "--root", str(root)],
                              capture_output=True, text=True)

    def test_unrecorded_removal_fails_and_recorded_removal_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.make_repo(root, [("v1.0.0", schema(["priority"])), ("v1.1.0", schema(None))])
            failed = self.run_check(root)
            self.assertEqual(failed.returncode, 1, failed.stdout)
            self.assertIn("not recorded", failed.stdout)
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            ledger = {"schema_version": 1, "removed": [{"key": "profiles.*.capacity.pools.*.mode",
                                                        "removed_in": "v1.1.0", "note": "delete mode from bundles"}]}
            self.make_repo(root, [("v1.0.0", schema(["priority"])), ("v1.1.0", schema(None))], ledger)
            passed = self.run_check(root)
            self.assertEqual(passed.returncode, 0, passed.stdout)

    def test_uncommitted_schema_removal_is_checked_in_working_tree(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.make_repo(root, [("v1.0.0", schema(["priority"])), ("v1.1.0", schema(["priority"]))])
            (root / "schemas" / "config-bundle.schema.json").write_text(json.dumps(schema(None)))
            result = self.run_check(root)
            self.assertEqual(result.returncode, 1, result.stdout)
            self.assertIn("capacity.pools.*.mode", result.stdout)

    def test_no_earlier_tag_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.make_repo(root, [("v1.0.0", schema(None))])
            result = self.run_check(root)
            self.assertEqual(result.returncode, 0, result.stdout)
            self.assertIn("no earlier release tag", result.stdout)


if __name__ == "__main__":
    unittest.main()
