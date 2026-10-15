"""Structured evidence extraction for the WeKnora MCP validation run.

The plan §6.4 / §7 / R6 require that document recall, evidence recall, and
citation support be computed from what the retrieval tools ACTUALLY returned
to the agent, not from a guess based on the final answer text. The legacy
`derive_tool_evidence` helper used a "first 20 characters of the required
evidence must appear in the tool text" rule, which both undercounts real
matches (a paraphrase in the model answer does not mean the tool was empty)
and overcounts (the same line in two documents would attribute hits to both).

This module replaces that with three primitives that share their parsing:

  * `parse_artifact_records(tool_calls, ...)` walks every successful
    ToolMessage whose `status == "success"` and pulls out the per-chunk,
    per-knowledge records the MCP server published. The contract for each
    tool is fixed; anything that does not match the schema is dropped.

  * `attribute_record(record, manifest_index, ...)` maps one parsed record
    back to a `logical_doc_id` (e.g. doc01) by joining the
    `knowledge_id` it carries to the manifest. Records that carry no
    `knowledge_id` (a free-text query result with no chunk) stay
    unattributed; they count toward the read-evidence denominator of the
    question they answered, never as a citation.

  * `summarize_records(records, required_evidence, ...)` returns the
    evidence view the report consumes: discovered documents, read
    evidence, and a per-chunk ledger that the manual review can audit
    one row at a time.

Sharing the parsing here means `run_eval` and `recompute_found` cannot
drift apart: they both call `summarize_records` over the same ToolMessage
streams.
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any, Iterable, Mapping, Sequence

# ToolMessage text payload that carries "knowledge_id" + "knowledge_base_id" +
# "chunk_id" in the same line is enough to attribute the chunk, but the
# artifact's structured_content.results is the authoritative source when
# present. Both paths are tried in order: structured first (no parsing loss),
# text fallback (older ToolMessage shape that we still want to recover).
EVIDENCE_TOOLS = frozenset({
    "search_knowledge", "grep_chunks", "read_document", "ask",
    "query_knowledge_graph",
    "wiki_search", "wiki_read_page", "wiki_index", "wiki_list_pages",
    "wiki_graph",
})

# Knowledge IDs are 36-char UUIDs; chunk IDs are too. Accept either to keep
# the matcher independent of any pre-1.0 schema decision.
_UUID_RE = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")


@dataclass
class EvidenceRecord:
    """One chunk the agent actually saw, with provenance intact.

    The fields are populated from the ToolMessage artifact; the
    `read_state` is one of "verified" (a quote or content was returned)
    or "unread" (the agent learned the chunk exists, but its content
    was not loaded in this call). A record with no `knowledge_id` is
    still useful — it counts toward the question's read evidence, just
    not as a citation against any particular document.
    """
    tool: str
    knowledge_id: str = ""
    knowledge_base_id: str = ""
    chunk_id: str = ""
    text: str = ""
    read_state: str = "unread"
    tool_call_id: str = ""
    extra: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        return {
            "tool": self.tool,
            "knowledge_id": self.knowledge_id,
            "knowledge_base_id": self.knowledge_base_id,
            "chunk_id": self.chunk_id,
            "text": self.text,
            "read_state": self.read_state,
            "tool_call_id": self.tool_call_id,
            "extra": self.extra,
        }


# --- artifact / text parsing --------------------------------------------------

def _tool_messages(raw_events: Sequence[Mapping[str, Any]]) -> Iterable[Mapping[str, Any]]:
    """Yield every ToolMessage-shaped event in capture order.

    The shape comes straight from mcp-go's `model_dump`: the ToolMessage
    itself has `type == "tool"`, `name == <tool>`, `status` either
    "success" or "error", `content` either a string or a list of content
    parts, and `artifact` a dict that carries structured_content when
    the server published it.
    """
    for ev in raw_events:
        if not isinstance(ev, Mapping):
            continue
        if ev.get("type") != "tool":
            continue
        yield ev


def _parse_artifact_results(artifact: Any) -> list[dict]:
    """Extract the per-chunk result rows from a ToolMessage artifact.

    Returns a list of dicts, one per result row. Each row at minimum
    carries `chunk_id` / `knowledge_id` / `knowledge_base_id` / `content`
    when the server published them. Rows that are pure metadata (a
    "type"/"role" envelope with no actual content) are kept but flagged
    unread so the report can show "model saw the id but not the text".
    """
    if not isinstance(artifact, Mapping):
        return []
    sc = artifact.get("structured_content")
    if not isinstance(sc, Mapping):
        return []
    results = sc.get("results")
    if not isinstance(results, list):
        return []
    out: list[dict] = []
    for r in results:
        if isinstance(r, Mapping):
            out.append(dict(r))
        elif isinstance(r, list):
            for sub in r:
                if isinstance(sub, Mapping):
                    out.append(dict(sub))
    return out


def _parse_text_chunks(text: str) -> list[dict]:
    """Fallback parser for the text content of a ToolMessage.

    The server does not always publish a structured_content artifact;
    in that case the chunk ids and contents are still recoverable from
    the text content, in the form of "<source knowledge_id='...'>" tags
    inserted by the agent tools. This parser only extracts what the text
    really carries, never guesses; missing knowledge_id -> an unread
    record with the text snippet, never a fabricated id.
    """
    if not text:
        return []
    # The agent tools write `<source knowledge_id="...">...</source>` for
    # every cited chunk. Pull the attributes + body.
    src_re = re.compile(
        r'<source\s+knowledge_id="(?P<kid>[0-9a-f-]{36})"(?:\s+knowledge_base_id="(?P<kbid>[0-9a-f-]{36})")?\s*>(?P<text>.*?)</source>',
        re.DOTALL,
    )
    out: list[dict] = []
    for m in src_re.finditer(text):
        text_body = m.group("text").strip()
        out.append({
            "knowledge_id": m.group("kid") or "",
            "knowledge_base_id": m.group("kbid") or "",
            "chunk_id": "",
            "content": text_body,
        })
    # Plain chunk_id=... lines (search/grep raw output, no <source> tags).
    if not out:
        for line in text.splitlines():
            cid = re.search(r"chunk_id[=:]?\s*`?([0-9a-f-]{36})", line)
            kid = re.search(r"knowledge_id[=:]?\s*`?([0-9a-f-]{36})", line)
            if cid or kid:
                out.append({
                    "knowledge_id": (kid.group(1) if kid else ""),
                    "knowledge_base_id": "",
                    "chunk_id": (cid.group(1) if cid else ""),
                    "content": line.strip(),
                })
    return out


def parse_artifact_records(
    raw_events: Sequence[Mapping[str, Any]],
    manifest_index: Mapping[str, str] | None = None,
) -> list[EvidenceRecord]:
    """Walk every ToolMessage, build a flat list of EvidenceRecord.

    Tool errors are dropped silently: a failed retrieval does not
    constitute evidence the model saw, and counting it would inflate
    "discovered" past what the agent actually has. The caller can still
    surface the error rate from the raw events for the report.
    """
    out: list[EvidenceRecord] = []
    for ev in _tool_messages(raw_events):
        tool = ev.get("name", "")
        if tool not in EVIDENCE_TOOLS:
            continue
        if ev.get("status") != "success":
            continue
        tool_call_id = ev.get("id") or ev.get("tool_call_id") or ""
        seen: set[tuple[str, str, str]] = set()
        artifact_rows = _parse_artifact_results(ev.get("artifact"))
        for r in artifact_rows:
            kid = r.get("knowledge_id") or r.get("knowledgeId") or ""
            kbid = r.get("knowledge_base_id") or r.get("knowledgeBaseId") or ""
            cid = r.get("chunk_id") or r.get("chunkId") or ""
            text = r.get("content") or r.get("text") or ""
            read_state = "verified" if text else "unread"
            key = (kid, kbid, cid)
            if key in seen:
                continue
            seen.add(key)
            out.append(EvidenceRecord(
                tool=tool, knowledge_id=kid, knowledge_base_id=kbid,
                chunk_id=cid, text=text, read_state=read_state,
                tool_call_id=tool_call_id, extra={"source": "artifact"},
            ))
        if artifact_rows:
            continue
        # Fallback: the server's text content carries the cited chunks.
        content = ev.get("content")
        text_str = ""
        if isinstance(content, str):
            text_str = content
        elif isinstance(content, list):
            for item in content:
                if isinstance(item, dict) and item.get("type") == "text":
                    text_str += item.get("text", "")
        for r in _parse_text_chunks(text_str):
            key = (r.get("knowledge_id", ""), r.get("knowledge_base_id", ""),
                   r.get("chunk_id", ""))
            if key in seen:
                continue
            seen.add(key)
            out.append(EvidenceRecord(
                tool=tool,
                knowledge_id=r.get("knowledge_id", ""),
                knowledge_base_id=r.get("knowledge_base_id", ""),
                chunk_id=r.get("chunk_id", ""),
                text=r.get("content", ""),
                read_state="verified" if r.get("content") else "unread",
                tool_call_id=tool_call_id,
                extra={"source": "text"},
            ))
    return out


# --- attribution + summary ---------------------------------------------------

def attribute_record(
    rec: EvidenceRecord, manifest_index: Mapping[str, str],
) -> str:
    """Map one record to a logical_doc_id or ''.

    `manifest_index` maps `logical_doc_id` -> real WeKnora `knowledge_id`
    (or the other way round: callers pass whichever they have; the index
    is checked in both directions). An empty string means "unattributed
    but real evidence"; the caller decides what to do with it.
    """
    if not rec.knowledge_id:
        return ""
    for logical, uuid_ in manifest_index.items():
        if uuid_ == rec.knowledge_id:
            return logical
    # Reverse map: caller might have passed {logical: uuid}; try the inverse.
    for uuid_, logical in manifest_index.items():
        if uuid_ == rec.knowledge_id:
            return logical
    return ""


def _read_state_for_evidence(records: Sequence[EvidenceRecord]) -> dict:
    """Return the evidence-side read summary.

    A "required_evidence" snippet is "read" only when a record with the
    same `text` actually appeared in the agent's tool output. We do not
    use the first-20-chars rule the old derivation used, because that
    rule misattributes snippets that share a prefix between two
    documents. Exact match is strict; if the corpus is the source of
    truth, the snippet is a unique quoted string and exact match is
    safe.
    """
    seen: set[str] = set()
    for r in records:
        if r.read_state == "verified" and r.text:
            seen.add(r.text.strip())
    return {"texts": seen}


def summarize_records(
    records: Sequence[EvidenceRecord],
    manifest_index: Mapping[str, str],
    required_evidence: Sequence[str] = (),
) -> dict:
    """Produce the fields the report + run_eval persist on a record.

    The shape is stable so the report renderer can be written once and
    reused as the schema grows:

      discovered_documents: logical ids the agent learned existed in
        the tool output (every artifact row counts, even unread ones,
        so a `discovered` claim is honest even when the model never
        opened the chunk).
      read_evidence_documents: logical ids the agent actually loaded
        text from (only `verified` records contribute).
      read_evidence_texts: the set of unique read texts; the report
        joins these against `required_evidence` for the strict evidence
        recall metric.
      hit_evidence: required_evidence items found verbatim in a
        `verified` record's text.
      unattributed: number of records that carried no `knowledge_id`
        or a `knowledge_id` not in the manifest; recorded so the
        operator can see when the corpus is too narrow.
      records: the full per-chunk ledger for the manual review.
    """
    discovered: set[str] = set()
    read_docs: set[str] = set()
    record_dicts: list[dict] = []
    unattributed = 0
    for r in records:
        logical = attribute_record(r, manifest_index)
        if not logical:
            unattributed += 1
        else:
            discovered.add(logical)
            if r.read_state == "verified":
                read_docs.add(logical)
        record_dicts.append(r.to_dict() | {"logical_doc_id": logical})
    read_texts = _read_state_for_evidence(records)["texts"]
    # Strip whitespace on the required_evidence so leading/trailing
    # whitespace in the corpus file does not break a legitimate match.
    required_clean = [e.strip() for e in (required_evidence or []) if e]
    hit_evidence = [e for e in required_clean if e and e in read_texts]
    return {
        "discovered_documents": sorted(discovered),
        "read_evidence_documents": sorted(read_docs),
        "read_evidence_texts": sorted(read_texts),
        "hit_evidence": hit_evidence,
        "unattributed_count": unattributed,
        "evidence_records": record_dicts,
    }


# --- shared derivation used by both run_eval and recompute_found -------------

def derive_evidence(
    raw_events: Sequence[Mapping[str, Any]],
    manifest_index: Mapping[str, str],
    required_evidence: Sequence[str] = (),
) -> dict:
    """The single entry point shared by run_eval and recompute_found.

    Pass the same `raw_events` to both, and both write the same
    discovered / read / hit fields. There is no second pass over the
    final answer here: the plan requires that the agent's tool output
    is the source of truth, not whatever the model chose to say.
    """
    records = parse_artifact_records(raw_events, manifest_index)
    return summarize_records(records, manifest_index, required_evidence)


# --- back-compat shim --------------------------------------------------------

# The legacy `derive_tool_evidence` is imported by `recompute_found.py` and
# `score_eval.py`; keep a thin shim that returns the same top-level keys the
# older code expects. New code should call `derive_evidence` directly.

def derive_tool_evidence(
    tool_calls: Sequence[Mapping[str, Any]],
    uuid2log: Mapping[str, str],
    required_evidence: Sequence[str] = (),
    corpus_index: Sequence[tuple[str, str]] | None = None,
) -> dict:
    """Back-compat shim for the legacy `derive_tool_evidence` API.

    It accepts the old `tool_calls` (already separated by `run_eval`)
    and the old `uuid2log` (`knowledge_id` -> `logical_doc_id`). The
    new derivation has no use for `corpus_index`; the exact-text rule
    on `required_evidence` removes the need to look up snippets in
    local files at all.
    """
    # Build a `raw_events`-shaped list so the shared parser does the work.
    raw_events = []
    for c in tool_calls or []:
        raw_events.append({
            "type": "tool",
            "name": c.get("name", ""),
            "status": c.get("status", ""),
            "content": c.get("content", ""),
            "artifact": c.get("artifact"),
            "id": c.get("tool_call_id") or c.get("id") or "",
        })
    # The legacy mapping was uuid->logical; the new derivation expects
    # logical->uuid (or both). Flip it once here.
    manifest_index = {logical: uuid_ for uuid_, logical in (uuid2log or {}).items()}
    summary = derive_evidence(raw_events, manifest_index, required_evidence)
    # The old report shape used docNN -> "docNN" form. Keep it.
    return {
        "found_documents": [d for d in summary["discovered_documents"]],
        "discovered_documents": summary["discovered_documents"],
        "read_evidence_documents": summary["read_evidence_documents"],
        "hit_evidence": summary["hit_evidence"],
        "cited_knowledge": sorted({r["knowledge_id"] for r in summary["evidence_records"]
                                    if r["knowledge_id"]}),
        "cited_chunks": sorted({r["chunk_id"] for r in summary["evidence_records"]
                                if r["chunk_id"]}),
        "evidence_records": summary["evidence_records"],
        "unattributed_count": summary["unattributed_count"],
    }
