#!/usr/bin/env python3
"""Prove make check-schemas rejects duplicate members at every nesting depth."""
import contextlib
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("check_schemas", ROOT / "scripts/check-schemas.py")
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class StrictSchemaJSON(unittest.TestCase):
    def test_duplicates_are_rejected_including_escaped_names(self):
        for text in ['{"description":"a","description":"b"}',
                     '{"properties":{"permissions":{"description":"a","description":"b"}}}',
                     r'{"description":"a","descript\u0069on":"b"}']:
            with self.subTest(text=text), self.assertRaisesRegex(ValueError, "duplicate JSON key"):
                checker.load_json(text)

    def test_same_key_in_different_objects_is_valid(self):
        checker.load_json('{"a":{"description":"a"},"b":{"description":"b"}}')

    def test_command_fails_for_duplicate_nested_schema_member(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            schemas = root / "schemas"
            schemas.mkdir()
            (schemas / "bad.json").write_text(
                '{"$id":"fixture","properties":{"permissions":{"description":"a","description":"b"}}}')
            errors = io.StringIO()
            with mock.patch.object(checker, "ROOT", root), mock.patch.object(checker, "SCHEMA_DIR", schemas), contextlib.redirect_stderr(errors):
                self.assertEqual(checker.main(), 1)
            self.assertIn("schemas/bad.json: invalid JSON (duplicate JSON key", errors.getvalue())


if __name__ == "__main__":
    unittest.main()
