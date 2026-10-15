"""Regression tests for the rewritten `evidence_records` parser.

The plan §6.4 / R6 require that document recall, evidence recall and
citation support be computed from the ToolMessage artifact the agent
actually saw, not from a guess based on the final answer text. These
tests pin the behavior the new shared parser must hold; any drift
between `run_eval` and `recompute_found` is a test failure, not a
silent recall jump.
"""
from __future__ import annotations

import sys
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))

import evidence_records as er  # noqa: E402


# A trivial manifest the parser can attribute against.
DOC1 = "11111111-1111-1111-1111-111111111111"
DOC2 = "22222222-2222-2222-2222-222222222222"
KB = "33333333-3333-3333-3333-333333333333"
MANIFEST = {"doc01": DOC1, "doc02": DOC2}


def _tool_event(name: str, status: str, artifact: dict | None = None,
                content: str = "", tool_call_id: str = "") -> dict:
    return {
        "type": "tool",
        "name": name,
        "status": status,
        "artifact": artifact,
        "content": content,
        "id": tool_call_id,
    }


def test_artifact_results_become_one_record_per_chunk():
    """The same quote in two different documents must not collapse into one
    citation. The first 20-character rule of the legacy derivation would
    attribute one hit to both documents; the new parser keeps the records
    distinct and lets the per-chunk ledger expose the duplication.
    """
    raw = [
        _tool_event("search_knowledge", "success", tool_call_id="t1", artifact={
            "structured_content": {
                "results": [
                    {"chunk_id": "c1", "knowledge_id": DOC1, "knowledge_base_id": KB,
                     "content": "项目A 依赖 服务B"},
                    {"chunk_id": "c2", "knowledge_id": DOC2, "knowledge_base_id": KB,
                     "content": "项目A 依赖 服务B"},
                ]
            }
        }),
    ]
    summary = er.derive_evidence(raw, MANIFEST)
    # Two distinct logical ids, each backed by one chunk.
    assert summary["discovered_documents"] == ["doc01", "doc02"]
    assert summary["read_evidence_documents"] == ["doc01", "doc02"]
    assert len(summary["evidence_records"]) == 2
    assert summary["hit_evidence"] == []
    # The two records carry different chunk_ids and different logical ids.
    seen_logical = sorted(r["logical_doc_id"] for r in summary["evidence_records"])
    assert seen_logical == ["doc01", "doc02"]


def test_error_tool_messages_drop_silently():
    """A tool whose ToolMessage reports `status=error` must not contribute
    to discovered / read evidence, no matter what its artifact looks like.
    Counting it would inflate recall past what the agent actually saw.
    """
    raw = [
        _tool_event("search_knowledge", "error", tool_call_id="t1", artifact={
            "structured_content": {
                "results": [{"chunk_id": "c1", "knowledge_id": DOC1,
                             "knowledge_base_id": KB, "content": "private"}]
            }
        }),
    ]
    summary = er.derive_evidence(raw, MANIFEST)
    assert summary["discovered_documents"] == []
    assert summary["read_evidence_documents"] == []
    assert summary["evidence_records"] == []


def test_required_evidence_hit_uses_strict_equality():
    """The hit_evidence set must come from exact equality with the
    required_evidence the question asked about. The legacy first-20-char
    rule overcounted; the new parser only matches the full stripped text.
    """
    raw = [
        _tool_event("read_document", "success", tool_call_id="t1", artifact={
            "structured_content": {
                "results": [{"chunk_id": "c1", "knowledge_id": DOC1,
                             "knowledge_base_id": KB, "content": "公开级"}]
            }
        }),
    ]
    # The required evidence includes a different snippet that happens to
    # share a prefix; strict equality rejects it.
    summary = er.derive_evidence(
        raw, MANIFEST,
        required_evidence=["公开级：所有员工可读", "机密级：禁止导出原文"],
    )
    # The chunk's text "公开级" is a strict prefix of "公开级：所有员工可读"
    # but is not the full required evidence; the new parser does not
    # substring-match, so the hit set is empty.
    assert summary["hit_evidence"] == []


def test_required_evidence_hit_matches_full_text():
    """The same strict equality that rejects prefixes must accept a full
    match: when the chunk's content is exactly the required_evidence
    fragment, the hit is recorded.
    """
    raw = [
        _tool_event("read_document", "success", tool_call_id="t1", artifact={
            "structured_content": {
                "results": [{"chunk_id": "c1", "knowledge_id": DOC1,
                             "knowledge_base_id": KB,
                             "content": "公开级：所有员工可读"}]
            }
        }),
    ]
    summary = er.derive_evidence(
        raw, MANIFEST,
        required_evidence=["公开级：所有员工可读"],
    )
    assert summary["hit_evidence"] == ["公开级：所有员工可读"]


def test_wiki_id_only_does_not_count_as_evidence():
    """A wiki_read_page ToolMessage that only emits source IDs (no quote,
    no text content) must not be counted as `read_evidence`. The previous
    derivation conflated "the page referenced this chunk" with "the agent
    read the chunk's text"; the new parser keeps them separate.
    """
    raw = [
        _tool_event("wiki_read_page", "success", tool_call_id="t1", artifact={
            "structured_content": {
                "results": [
                    # No `content` field, no `text` field — ID only.
                    {"chunk_id": "c1", "knowledge_id": DOC1,
                     "knowledge_base_id": KB, "knowledge_base_id_source": "wiki"},
                ]
            }
        }),
    ]
    summary = er.derive_evidence(raw, MANIFEST)
    # Discovered includes the KB; read does not (no verified text).
    assert "doc01" in summary["discovered_documents"]
    assert summary["read_evidence_documents"] == []
    assert summary["evidence_records"][0]["read_state"] == "unread"


def test_tool_call_id_survives_into_records():
    """The plan requires per-record tool_call_id so the manual review
    can cross-reference each chunk back to the originating tool call.
    """
    raw = [
        _tool_event("search_knowledge", "success", tool_call_id="abc-123", artifact={
            "structured_content": {
                "results": [{"chunk_id": "c1", "knowledge_id": DOC1,
                             "knowledge_base_id": KB, "content": "x"}]
            }
        }),
    ]
    summary = er.derive_evidence(raw, MANIFEST)
    assert summary["evidence_records"][0]["tool_call_id"] == "abc-123"


def test_unattributed_records_are_counted_but_not_claimed():
    """A record whose knowledge_id is not in the manifest still counts
    as evidence the agent saw; it just does not get attributed to a
    logical doc id. The corpus is too narrow when many records fall
    in this bucket, and the operator should see the count.
    """
    raw = [
        _tool_event("search_knowledge", "success", tool_call_id="t1", artifact={
            "structured_content": {
                "results": [{"chunk_id": "c1",
                             "knowledge_id": "ffffffff-ffff-ffff-ffff-ffffffffffff",
                             "knowledge_base_id": KB, "content": "out of scope"}]
            }
        }),
    ]
    summary = er.derive_evidence(raw, MANIFEST)
    assert summary["discovered_documents"] == []
    assert summary["unattributed_count"] == 1
    assert summary["evidence_records"][0]["logical_doc_id"] == ""


def test_text_fallback_recovers_source_tag_chunks():
    """When the server does not publish a structured_content artifact,
    the parser must still recover the cited chunks from the
    `<source knowledge_id="...">...</source>` tags the agent tools
    insert into the text content.
    """
    raw = [
        _tool_event("wiki_read_page", "success", tool_call_id="t1", artifact=None,
                    content=(
                        f'<source knowledge_id="{DOC1}" '
                        f'knowledge_base_id="{KB}">'
                        f'项目A 负责人 张三</source>'
                    )),
    ]
    summary = er.derive_evidence(raw, MANIFEST)
    assert summary["discovered_documents"] == ["doc01"]
    assert "doc01" in summary["read_evidence_documents"]
    assert summary["hit_evidence"] == []


def test_legacy_shim_returns_legacy_keys():
    """`derive_tool_evidence` is the legacy entry point used by older
    scripts; it must keep returning the keys downstream callers expect
    while delegating to the new shared parser.
    """
    raw_events = [
        _tool_event("search_knowledge", "success", tool_call_id="t1", artifact={
            "structured_content": {
                "results": [{"chunk_id": "c1", "knowledge_id": DOC1,
                             "knowledge_base_id": KB, "content": "项目A"}]
            }
        }),
    ]
    # The legacy shim accepts `tool_calls` (already separated); rebuild
    # it from the raw_events list so the public surface keeps working.
    tool_calls = [{
        "name": e["name"], "content": e["content"], "status": e["status"],
        "artifact": e["artifact"], "tool_call_id": e["id"],
    } for e in raw_events if e["type"] == "tool"]
    uuid2log = {DOC1: "doc01"}
    out = er.derive_tool_evidence(tool_calls, uuid2log)
    assert out["found_documents"] == ["doc01"]
    assert out["cited_knowledge"] == [DOC1]
    assert out["cited_chunks"] == ["c1"]
    assert "evidence_records" in out
    assert "discovered_documents" in out
