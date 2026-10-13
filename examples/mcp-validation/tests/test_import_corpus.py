"""Unit tests for import_corpus.py's pure helpers (no live WeKnora needed)."""
import hashlib
import json
import sys
from pathlib import Path

import pytest

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))

import import_corpus as ic  # noqa: E402


def test_idempotency_key_is_deterministic():
    a = ic.idempotency_key("kb-1", "abc123")
    b = ic.idempotency_key("kb-1", "abc123")
    c = ic.idempotency_key("kb-1", "def456")
    d = ic.idempotency_key("kb-2", "abc123")
    assert a == b, "same (kb, sha) must produce the same key"
    assert a != c, "different bytes must produce a different key"
    assert a != d, "different KB must produce a different key"


def test_sha256_of_matches_manifest_convention():
    content = b"hello weknora\n"
    tmp = Path(__file__).with_name("_sha_fixture.txt")
    tmp.write_bytes(content)
    try:
        got = ic.sha256_of(tmp)
        assert got == hashlib.sha256(content).hexdigest()
    finally:
        tmp.unlink()


def test_atomic_write_manifest_keeps_structure(tmp_path):
    target = tmp_path / "manifest.json"
    target.write_text(json.dumps({"documents": [{"id": "doc01"}]}), encoding="utf-8")
    ic.atomic_write_manifest(target, {"documents": [{"id": "doc01", "weknora_document_id": "u1"}]})
    data = json.loads(target.read_text(encoding="utf-8"))
    assert data["documents"][0]["weknora_document_id"] == "u1"
    # The temp file must not linger after a successful replace.
    assert not (tmp_path / "manifest.json.tmp").exists()
