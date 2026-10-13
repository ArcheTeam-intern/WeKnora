#!/usr/bin/env python3
"""End-to-end offline evaluation against the real WeKnora MCP + a real model.

For every question in eval/questions/demo-12.json this script:
  1. asks the standard LangChain Agent (create_agent + MCP tools) the question,
  2. captures the raw AIMessage / ToolMessage events (no normalization),
  3. resolves the cited knowledge_ids / chunk_ids / wiki slugs / relations
     back to the corpus manifest's logical_doc_id set,
  4. emits one JSONL record per question so score_eval.py can compute the
     plan §8 metrics (document recall, evidence recall, fact coverage,
     citation support).

Usage (§11.3)::

    python scripts/run_eval.py \
        --agent-dir E:/GoLandCode/WeKnora-Agent \
        --manifest eval/corpus/manifest.json \
        --questions eval/questions/demo-12.json \
        --output-dir eval/reports
"""
import argparse
import asyncio
import json
import os
import re
import sys
import time
import uuid
from pathlib import Path

# Make the Agent's agent.py importable when run_eval.py is invoked from any cwd.
_AGENT_DIR = Path(os.environ.get("WEKNORA_AGENT_DIR", "E:/GoLandCode/WeKnora-Agent"))
if str(_AGENT_DIR) not in sys.path:
    sys.path.insert(0, str(_AGENT_DIR))

from dotenv import load_dotenv
from langchain_mcp_adapters.client import MultiServerMCPClient
from langchain.agents import create_agent


def need(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise RuntimeError(f"missing environment variable: {name}")
    return value


def connection() -> dict:
    kb_ids = [s.strip() for s in need("WEKNORA_KB_IDS").split(",") if s.strip()]
    return {"weknora": {
        "transport": "streamable_http",
        "url": need("WEKNORA_MCP_URL"),
        "headers": {
            "Authorization": "Bearer " + need("WEKNORA_MCP_TOKEN"),
            "X-WeKnora-User-Token": need("WEKNORA_USER_TOKEN"),
            "X-Tenant-ID": need("WEKNORA_WORKSPACE_ID"),
            "X-WeKnora-Knowledge-Base-Ids": ",".join(kb_ids),
        },
    }}


def load_manifest(path: Path) -> dict:
    with path.open(encoding="utf-8") as stream:
        return json.load(stream)


def load_questions(path: Path) -> list:
    with path.open(encoding="utf-8") as stream:
        return json.load(stream).get("questions", [])


# doc01 … docN -> WeKnora UUID (one WeKnora KB may host several logical
# documents; the manifest carries the real id once an import_corpus run
# has back-filled it).
def manifest_index(manifest: dict) -> dict[str, str]:
    out = {}
    for d in manifest.get("documents", []):
        logical = d.get("logical_doc_id") or d.get("doc_id")
        uuid_ = d.get("weknora_document_id") or d.get("imported_document_id")
        if logical and uuid_:
            out[logical] = uuid_
    return out


# Extract any UUID-shaped or docNN-shaped identifier the model cited, so the
# downstream metrics can count actual recall rather than inferred recall.
_UUID_RE = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_DOCID_RE = re.compile(r"\bdoc(\d{2})\b")
_CHUNKID_RE = re.compile(r"chunk_id[=:]?\s*([0-9a-f-]{36})")
_KNOWLEDGEID_RE = re.compile(r"knowledge_id[=:]?\s*([0-9a-f-]{36})")
_SLUG_RE = re.compile(r"wiki[_\s][Pp]age|slug[=:]?\s*[`'\"]([a-z0-9-]+)[`'\"]]")


def find_cited_ids(answer: str) -> dict[str, set[str]]:
    return {
        "doc_logical": set(f"doc{n}" for n in _DOCID_RE.findall(answer)),
        "knowledge": set(_KNOWLEDGEID_RE.findall(answer)) | set(_UUID_RE.findall(answer)),
        "chunk": set(_CHUNKID_RE.findall(answer)),
    }


# The model often enumerates the search SCOPE in its answer ("I searched
# docs 1–10"), which is NOT the same as the docs it CITES. found_documents
# must be the latter: only docNN tokens that appear inside the citation
# block, where the model actually invokes a quote or an id.
#
# A "citation block" is any line that carries at least one of:
#   knowledge_id <uuid>
#   chunk_id <uuid>
#   `knowledge_base_id`  (the MCP output name)
#   a quoted phrase
# DocNN tokens in plain prose ("范围是 doc01..doc10", "从以下文档中检索")
# do NOT count as citations.
def extract_cited_doc_logicals(answer: str) -> set[str]:
    cited: set[str] = set()
    for line in answer.splitlines():
        ids = _DOCID_RE.findall(line)
        if not ids:
            continue
        if "knowledge_id" not in line and not _CHUNKID_RE.search(line) \
                and "knowledge_base_id" not in line \
                and "knowledge base" not in line.lower():
            continue
        for n in ids:
            cited.add(f"doc{n}")
    for m in re.finditer(r"doc(\d{2})\b[^a-z\n]{0,40}knowledge_id", answer):
        cited.add(f"doc{m.group(1)}")
    for m in re.finditer(r"knowledge_id[^a-z\n]{0,40}doc(\d{2})\b", answer):
        cited.add(f"doc{m.group(1)}")
    for m in re.finditer(r"doc(\d{2})\b[^a-z\n]{0,40}chunk_id", answer):
        cited.add(f"doc{m.group(1)}")
    return cited


async def ask_agent(client, question: str) -> dict:
    """Single Agent ainvoke, returns raw messages + final answer text."""
    # Build the agent inside this call so each question gets a fresh tool
    # discovery; otherwise a tool list change between questions is missed.
    tools = await client.get_tools()
    from agent import READ_ONLY_TOOLS, SYSTEM_PROMPT  # noqa: WPS433 - top-level import kept inside to make the import path explicit
    tools = [t for t in tools if t.name in READ_ONLY_TOOLS]
    model_name = need("AGENT_MODEL")
    from langchain_openai import ChatOpenAI
    model = ChatOpenAI(
        model=model_name,
        base_url=need("AGENT_MODEL_BASE_URL"),
        api_key=need("AGENT_MODEL_API_KEY"),
        temperature=0,
        timeout=120,
        max_retries=0,
    )
    agent = create_agent(model, tools, system_prompt=SYSTEM_PROMPT)
    raw_events = []
    tool_calls: list[dict] = []
    try:
        async with asyncio.timeout(180):
            result = await agent.ainvoke(
                {"messages": [{"role": "user", "content": question}]},
                config={"recursion_limit": 20},
            )
        messages = result.get("messages", []) if isinstance(result, dict) else []
        for m in messages:
            try:
                entry = m.model_dump(mode="json", by_alias=True)
            except Exception:  # noqa: BLE001 - some message types don't dump
                entry = {"type": type(m).__name__,
                        "content": getattr(m, "content", "")}
            raw_events.append(entry)
            if getattr(m, "type", "") == "tool":
                tool_calls.append({
                    "name": getattr(m, "name", ""),
                    "content": getattr(m, "content", ""),
                    "status": getattr(m, "status", ""),
                    "artifact": getattr(m, "artifact", None),
                })
        final = messages[-1].content if messages else ""
    except Exception as exc:  # noqa: BLE001 - capture per-question failures
        final = f"[ERROR] {type(exc).__name__}: {exc}"
    return {
        "raw_events": raw_events,
        "tool_calls": tool_calls,
        "final": final if isinstance(final, str) else str(final),
    }


# Tools whose output carries document evidence. Discovery and status tools
# (list_knowledge_bases, list_documents, get_*_status) enumerate every
# document id in scope, so counting their output would make "found" mean
# "listed" rather than "retrieved" and document recall would collapse to
# whether a listing call ran. Only retrieval/read outputs prove the model
# pulled a document's evidence in (plan §8).
EVIDENCE_TOOLS = frozenset({
    "search_knowledge", "grep_chunks", "read_document", "ask",
    "query_knowledge_graph",
    "wiki_search", "wiki_read_page", "wiki_index", "wiki_list_pages",
    "wiki_graph",
})


def _tool_text(tool_calls: list[dict], names=None) -> str:
    parts = []
    for c in tool_calls:
        if names is not None and c.get("name") not in names:
            continue
        content = c.get("content")
        if isinstance(content, str):
            parts.append(content)
        elif isinstance(content, list):
            for item in content:
                if isinstance(item, dict) and item.get("type") == "text":
                    parts.append(item.get("text", ""))
    return "\n".join(parts)


def corpus_evidence_index(manifest: dict, corpus_dir: Path) -> list[tuple[str, str]]:
    """(doc_text, logical_doc_id) pairs for every corpus file in the manifest.

    search_knowledge's text output carries chunk content but not the parent
    document's UUID, so UUID reverse-resolution alone undercounts documents
    the model really retrieved. A hit evidence snippet is itself proof of
    retrieval, and the local corpus says which document each snippet belongs
    to — that attribution is exact for this corpus, not a heuristic.
    """
    index = []
    for d in manifest.get("documents", []):
        logical = d.get("logical_doc_id") or d.get("doc_id")
        path = corpus_dir / (d.get("file_name") or "")
        if not logical or not path.is_file():
            continue
        index.append((path.read_text(encoding="utf-8"), logical))
    return index


def derive_tool_evidence(
    tool_calls: list[dict], uuid2log: dict[str, str], required_evidence: list,
    corpus_index: list[tuple[str, str]] | None = None,
) -> dict:
    """Derive the found/cited/hit fields from the tool evidence only.

    One function so run_eval (at capture time) and recompute_found (on a
    saved runs.jsonl) cannot drift apart.
    """
    tool_text = _tool_text(tool_calls, EVIDENCE_TOOLS)
    found_doc_ids: set[str] = set()
    cited_knowledge: set[str] = set()
    for uuid_ in _UUID_RE.findall(tool_text):
        cited_knowledge.add(uuid_)
        logical = uuid2log.get(uuid_)
        if logical:
            found_doc_ids.add(logical.replace("doc", ""))
    # Evidence recall: a required_evidence snippet is "read" when it appears
    # inside the tool-returned content (the model actually saw it), not when
    # the model paraphrases it in the answer.
    hit_evidences = [e for e in required_evidence
                     if e and e[:20] in tool_text]
    # A snippet that only exists in document X cannot appear in the tool
    # output unless X's content was retrieved, so a hit attributes X directly
    # — this closes the search_knowledge gap where the text output shows the
    # chunk but not its document UUID.
    for evidence in hit_evidences:
        head = evidence[:20]
        for doc_text, logical in (corpus_index or []):
            if head in doc_text:
                found_doc_ids.add(logical.replace("doc", ""))
    return {
        "found_documents": sorted(found_doc_ids),
        "cited_knowledge": sorted(cited_knowledge),
        "cited_chunks": sorted(set(_CHUNKID_RE.findall(tool_text))),
        "hit_evidence": hit_evidences,
    }


def build_record(
    question: dict, manifest_uuids: dict, run: dict, started: float,
    corpus_index: list[tuple[str, str]] | None = None,
) -> dict:
    final = run["final"]
    tool_calls = run["tool_calls"]

    # Found documents come from the tool evidence, NOT from the final answer:
    # a knowledge_id UUID the retrieval tools actually returned is
    # reverse-resolved through the manifest back to a logical docNN. This is
    # what "document recall" means — the model must have pulled the document's
    # evidence in. Discovery/status tools are excluded: they list every id in
    # scope, which would turn "found" into "listed".
    uuid2log = {v: k for k, v in manifest_uuids.items()}
    evidence = derive_tool_evidence(tool_calls, uuid2log,
                                    question.get("required_evidence", []),
                                    corpus_index)
    found_doc_ids = evidence["found_documents"]
    # The final answer's own docNN citations are kept separately (they are
    # "the model claims to cite docNN"), never merged into found_documents.
    answer_cited_docs = extract_cited_doc_logicals(final)

    record = {
        "question_id": question["id"],
        "category": question.get("category", ""),
        "question": question.get("question", ""),
        "answerable": bool(question.get("answerable", True)),
        "required_documents": sorted(question.get("relevant_documents") or []),
        "found_documents": evidence["found_documents"],
        "answer_cited_documents": sorted(answer_cited_docs),
        "required_evidence": question.get("required_evidence", []),
        "hit_evidence": evidence["hit_evidence"],
        "cited_knowledge": evidence["cited_knowledge"],
        "cited_chunks": evidence["cited_chunks"],
        "final_answer": final,
        "tool_statuses": [c.get("status") for c in tool_calls],
        "tool_call_names": [c.get("name") for c in tool_calls],
        "tool_error": any(c.get("status") == "error" for c in tool_calls),
        # The raw messages (AIMessage tool_calls + ToolMessage content/artifact/
        # status) are the reviewable evidence the plan requires; keep them so a
        # human can verify each citation against the actual tool output.
        "raw_events": run["raw_events"],
        "elapsed_sec": round(time.monotonic() - started, 2),
    }
    return record


async def main_async(args) -> int:
    manifest = load_manifest(Path(args.manifest))
    questions = load_questions(Path(args.questions))
    manifest_uuids = manifest_index(manifest)
    corpus_index = corpus_evidence_index(manifest, Path(args.manifest).parent)
    client = MultiServerMCPClient(connection())
    out_dir = Path(args.output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    runs_path = out_dir / "runs.jsonl"
    failed = 0
    with runs_path.open("w", encoding="utf-8") as stream:
        for q in questions:
            started = time.monotonic()
            print(f"[{q['id']}] {q['category']} -> {q['question'][:50]}…")
            run = await ask_agent(client, q["question"])
            rec = build_record(q, manifest_uuids, run, started, corpus_index)
            stream.write(json.dumps(rec, ensure_ascii=False) + "\n")
            stream.flush()
            if "[ERROR]" in rec["final_answer"]:
                failed += 1
                print(f"  FAIL: {rec['final_answer'][:120]}")
            else:
                print(f"  ok ({len(rec['raw_events'])} events, "
                      f"{len(rec['tool_call_names'])} tool calls, "
                      f"{rec['elapsed_sec']}s)")
    print(f"\nruns.jsonl: {runs_path} ({len(questions) - failed}/{len(questions)} ok)")
    return 0 if failed == 0 else 1


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description="Run the 12-question eval")
    parser.add_argument("--agent-dir", type=Path,
                        default=os.environ.get("WEKNORA_AGENT_DIR", "E:/GoLandCode/WeKnora-Agent"))
    parser.add_argument("--manifest", type=Path,
                        default=root / "eval/corpus/manifest.json")
    parser.add_argument("--questions", type=Path,
                        default=root / "eval/questions/demo-12.json")
    parser.add_argument("--output-dir", type=Path,
                        default=root / "eval/reports")
    args = parser.parse_args()
    load_dotenv(Path(args.agent_dir) / ".env", override=False)
    return asyncio.run(main_async(args))


if __name__ == "__main__":
    raise SystemExit(main())
