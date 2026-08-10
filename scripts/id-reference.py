#!/usr/bin/env python3
"""Verify the frozen version-1 typed six-word ID contract independently.

This reader intentionally does not import the Go implementation.  It loads the
repository-owned word list and golden vectors, then implements the wire format
and type-bound checksum with only Python's standard library.  Keeping this
small verifier separate from the production package gives the fixture a
cross-language compatibility check.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_WORDLIST = ROOT / "internal" / "ids" / "wordlist.txt"
DEFAULT_FIXTURE = ROOT / "tests" / "fixtures" / "ids-v1.json"

DOMAIN = b"agentctl-word-id-v1\x00"
PAYLOAD_MASK = (1 << 60) - 1
ENCODING_VERSION = 1
REGISTERED_TYPES = (
    "exec",
    "event",
    "sub",
    "cursor",
    "delivery",
    "artifact",
    "context",
    "route",
    "host",
    "repo",
    "knowledge",
    "source",
    "project",
    "issue",
    "run",
)
WORD_PATTERN = re.compile(r"^[a-z]{3,10}$")


class InvalidID(ValueError):
    """A candidate that violates the version-1 ID contract."""

    def __init__(self, reason: str, value: str) -> None:
        super().__init__(f"{reason}: {value!r}")
        self.reason = reason


class VerificationError(ValueError):
    """A malformed fixture or a failed golden-vector assertion."""


def checksum(typ: str, payload: int) -> int:
    digest = hashlib.sha256(
        DOMAIN + typ.encode("utf-8") + b"\x00" + payload.to_bytes(8, "big")
    ).digest()
    return digest[0] >> 2


def encode(typ: str, payload: int, words: list[str]) -> str:
    if typ not in REGISTERED_TYPES:
        raise VerificationError(f"unregistered type prefix {typ!r}")
    if isinstance(payload, bool) or not isinstance(payload, int):
        raise VerificationError(f"payload is not an integer for {typ!r}")
    if not 0 <= payload <= PAYLOAD_MASK:
        raise VerificationError(f"payload exceeds 60 bits for {typ!r}: {payload}")

    indices = (
        (payload >> 49) & 0x7FF,
        (payload >> 38) & 0x7FF,
        (payload >> 27) & 0x7FF,
        (payload >> 16) & 0x7FF,
        (payload >> 5) & 0x7FF,
        ((payload & 0x1F) << 6) | checksum(typ, payload),
    )
    return "-".join((typ, *(words[index] for index in indices)))


def decode(value: str, word_index: dict[str, int], words: list[str]) -> tuple[str, int]:
    if not isinstance(value, str):
        raise InvalidID("ID is not a string", repr(value))
    if not value or value.lower() != value:
        raise InvalidID("ID must be canonical lowercase", value)

    parts = value.split("-")
    if len(parts) != 7:
        raise InvalidID("ID must contain a type and six words", value)
    typ = parts[0]
    if typ not in REGISTERED_TYPES:
        raise InvalidID("unregistered type prefix", value)

    indices: list[int] = []
    for word in parts[1:]:
        index = word_index.get(word)
        if index is None:
            raise InvalidID("word is not in word list v1", value)
        indices.append(index)

    payload = (
        (indices[0] << 49)
        | (indices[1] << 38)
        | (indices[2] << 27)
        | (indices[3] << 16)
        | (indices[4] << 5)
        | (indices[5] >> 6)
    )
    want = indices[5] & 0x3F
    got = checksum(typ, payload)
    if got != want:
        raise InvalidID("type-bound checksum mismatch", value)

    canonical = encode(typ, payload, words)
    if canonical != value:
        raise InvalidID("noncanonical encoding", value)
    return typ, payload


def load_words(path: Path) -> tuple[list[str], dict[str, int], str]:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise VerificationError(f"cannot read word list {path}: {exc}") from exc
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise VerificationError(f"word list is not UTF-8: {exc}") from exc

    if not raw.endswith(b"\n"):
        raise VerificationError("word list must end with a newline")
    words = text.removesuffix("\n").split("\n")
    if len(words) != 2048:
        raise VerificationError(f"word list has {len(words)} words, expected 2048")
    if any(not WORD_PATTERN.fullmatch(word) for word in words):
        raise VerificationError("word list contains a word outside [a-z]{3,10}")
    if len(set(words)) != len(words):
        raise VerificationError("word list contains duplicate words")

    digest = "sha256:" + hashlib.sha256(raw).hexdigest()
    return words, {word: index for index, word in enumerate(words)}, digest


def load_fixture(path: Path) -> dict[str, Any]:
    try:
        fixture = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise VerificationError(f"cannot read fixture {path}: {exc}") from exc
    if not isinstance(fixture, dict):
        raise VerificationError("fixture root must be an object")
    return fixture


def require_field(document: dict[str, Any], field: str) -> Any:
    if field not in document:
        raise VerificationError(f"fixture is missing {field!r}")
    return document[field]


def verify_fixture(
    fixture: dict[str, Any],
    words: list[str],
    word_index: dict[str, int],
    digest: str,
) -> tuple[int, int]:
    version = require_field(fixture, "encoding_version")
    if (
        isinstance(version, bool)
        or not isinstance(version, int)
        or version != ENCODING_VERSION
    ):
        raise VerificationError("fixture encoding_version is not 1")
    if require_field(fixture, "word_list_digest") != digest:
        raise VerificationError(
            f"fixture word_list_digest does not match wordlist.txt ({digest})"
        )

    vectors = require_field(fixture, "vectors")
    if not isinstance(vectors, list) or not vectors:
        raise VerificationError("fixture vectors must be a non-empty array")
    seen: set[tuple[str, int]] = set()
    for number, vector in enumerate(vectors, start=1):
        if not isinstance(vector, dict):
            raise VerificationError(f"vector {number} must be an object")
        typ = vector.get("type")
        payload = vector.get("payload")
        value = vector.get("id")
        if not isinstance(typ, str) or typ not in REGISTERED_TYPES:
            raise VerificationError(f"vector {number} has an invalid type")
        if isinstance(payload, bool) or not isinstance(payload, int):
            raise VerificationError(f"vector {number} payload must be an integer")
        if not isinstance(value, str):
            raise VerificationError(f"vector {number} id must be a string")
        key = (typ, payload)
        if key in seen:
            raise VerificationError(f"duplicate vector {typ!r}/{payload}")
        seen.add(key)

        expected = encode(typ, payload, words)
        if expected != value:
            raise VerificationError(
                f"vector {number} encode mismatch: got {value!r}, expected {expected!r}"
            )
        try:
            decoded_type, decoded_payload = decode(value, word_index, words)
        except InvalidID as exc:
            raise VerificationError(f"vector {number} does not decode: {exc}") from exc
        if (decoded_type, decoded_payload) != key:
            raise VerificationError(
                f"vector {number} decoded to {decoded_type!r}/{decoded_payload}"
            )

    rejected = require_field(fixture, "reject")
    if not isinstance(rejected, dict):
        raise VerificationError("fixture reject must be an object")
    rejection_count = 0
    for category in ("wrong_type", "casing", "checksum"):
        values = rejected.get(category)
        if not isinstance(values, list) or not values:
            raise VerificationError(f"fixture reject.{category} must be a non-empty array")
        for value in values:
            if not isinstance(value, str):
                raise VerificationError(f"fixture reject.{category} contains a non-string")
            try:
                decode(value, word_index, words)
            except InvalidID as exc:
                if category == "wrong_type" and exc.reason != "type-bound checksum mismatch":
                    raise VerificationError(
                        f"wrong_type fixture was rejected for {exc.reason}, not its checksum"
                    ) from exc
                if category == "casing" and exc.reason != "ID must be canonical lowercase":
                    raise VerificationError(
                        f"casing fixture was rejected for {exc.reason}, not its case"
                    ) from exc
                if category == "checksum" and exc.reason != "type-bound checksum mismatch":
                    raise VerificationError(
                        f"checksum fixture was rejected for {exc.reason}, not its checksum"
                    ) from exc
            else:
                raise VerificationError(f"reject.{category} was accepted: {value!r}")
            rejection_count += 1

    return len(vectors), rejection_count


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wordlist", type=Path, default=DEFAULT_WORDLIST)
    parser.add_argument("--fixture", type=Path, default=DEFAULT_FIXTURE)
    args = parser.parse_args()

    try:
        words, word_index, digest = load_words(args.wordlist)
        fixture = load_fixture(args.fixture)
        vector_count, rejection_count = verify_fixture(
            fixture, words, word_index, digest
        )
    except VerificationError as exc:
        print(f"id reference check failed: {exc}", file=sys.stderr)
        return 1

    print(
        "id reference check passed "
        f"(encoding v{ENCODING_VERSION}, {vector_count} vectors, {rejection_count} rejected candidates)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
