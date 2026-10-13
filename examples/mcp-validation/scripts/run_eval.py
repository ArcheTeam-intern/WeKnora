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
    cited: list[dict] = []
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
                cited.append({
                    "name": getattr(m, "name", ""),
                    "content": getattr(m, "content", ""),
                })
        final = messages[-1].content if messages else ""
    except Exception as exc:  # noqa: BLE001 - capture per-question failures
        final = f"[ERROR] {type(exc).__name__}: {exc}"
    return {
        "raw_events": raw_events,
        "tool_calls": cited,
        "final": final if isinstance(final, str) else str(final),
    }


def build_record(question: dict, manifest_uuids: dict, run: dict, started: float) -> dict:
    final = run["final"]
    cited = find_cited_ids(final)
    cited_docs = extract_cited_doc_logicals(final) | cited["doc_logical"]
    relevant = set(question.get("relevant_documents") or [])
    answerable = bool(question.get("answerable", True))
    # 标记「required_evidence 是否被字面引用」——粗略判据，真实支持由人工复核。
    text = final
    hit_evidences = [e for e in question.get("required_evidence", [])
                     if e and e[:20] in text]
    record = {
        "question_id": question["id"],
        "category": question.get("category", ""),
        "question": question.get("question", ""),
        "answerable": answerable,
        "required_documents": sorted(relevant),
        "found_documents": sorted(cited_docs),
        "required_evidence": question.get("required_evidence", []),
        "hit_evidence": hit_evidences,
        "cited_knowledge": sorted(cited["knowledge"]),
        "cited_chunks": sorted(cited["chunk"]),
        "final_answer": final,
        "raw_event_count": len(run["raw_events"]),
        "tool_call_names": [c["name"] for c in run["tool_calls"]],
        "elapsed_sec": round(time.monotonic() - started, 2),
    }
    return record


async def main_async(args) -> int:
    manifest = load_manifest(Path(args.manifest))
    questions = load_questions(Path(args.questions))
    manifest_uuids = manifest_index(manifest)
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
            rec = build_record(q, manifest_uuids, run, started)
            stream.write(json.dumps(rec, ensure_ascii=False) + "\n")
            stream.flush()
            if "[ERROR]" in rec["final_answer"]:
                failed += 1
                print(f"  FAIL: {rec['final_answer'][:120]}")
            else:
                print(f"  ok ({rec['raw_event_count']} events, "
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
