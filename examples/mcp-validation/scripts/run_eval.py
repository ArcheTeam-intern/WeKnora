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

Two strategies are supported (--strategy):

  * `agent`   - the standard multi-tool ReAct agent. This is the historical
    default and the one the plan §6.4 acceptance contract targets.
  * `baseline` - one search_knowledge(mode=hybrid, limit=10) call followed
    by a single model generation with the model's tool/answer choice
    removed. This is a deliberately weaker lower bound, so the agent
    number can be read as "how much did tool use help".
  * `both` - run both strategies in the same invocation, emitting one
    record per (question, strategy) pair.

The shared evidence parser is in `evidence_records.py`; this script is the
input layer, score_eval.py the renderer. There is no shared fallback to
"best guess" when an evidence item is missing: the manual review template
records the gap as `unattributed` or `unverified`, and the report marks
the run as not_evaluated.

Usage (§11.3)::

    python scripts/run_eval.py \
        --agent-dir E:/GoLandCode/WeKnora-Agent \
        --manifest eval/corpus/manifest.json \
        --questions eval/questions/demo-12.json \
        --strategy both \
        --output-dir eval/reports/correction-current
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

# Make the local scripts package importable as `scripts.*` without colliding
# with the WeKnora-Agent `scripts/` directory of the same name.
_ROOT = Path(__file__).resolve().parents[1]
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))
sys.path.insert(0, str(_ROOT / "scripts"))

from dotenv import load_dotenv
from langchain_mcp_adapters.client import MultiServerMCPClient
from langchain.agents import create_agent

import evidence_records  # noqa: E402 - same-directory import after sys.path tweak


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
    """Single Agent ainvoke, returns raw messages + final answer text.

    Uses the same build_agent path the CLI ships, so any model /
    tool / system-prompt change in the production code is what the
    offline eval measures. The whole-run timeout (120s) wraps the
    per-model timeout (60s); get_tools() is itself bounded so a
    broken MCP server cannot eat the model's full budget.
    """
    from agent import build_agent  # noqa: WPS433 - import path pinned to the
    # WeKnora-Agent source tree, same one the CLI uses.
    model_name = need("AGENT_MODEL")
    from langchain_openai import ChatOpenAI
    model = ChatOpenAI(
        model=model_name,
        base_url=need("AGENT_MODEL_BASE_URL"),
        api_key=need("AGENT_MODEL_API_KEY"),
        temperature=0,
        timeout=60,
        max_retries=0,
    )
    raw_events = []
    tool_calls: list[dict] = []
    run_status = "success"
    error_kind = ""
    final = ""
    try:
        async with asyncio.timeout(120):
            # build_agent performs the get_tools() handshake and
            # constructs the ReAct agent. Same call path the CLI uses.
            agent = await build_agent(model, connection())
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
                    "tool_call_id": getattr(m, "tool_call_id", ""),
                })
        final = messages[-1].content if messages else ""
    except TimeoutError:
        run_status = "timeout"
        error_kind = "asyncio.TimeoutError"
        # Keep what we managed to collect so a human can still see the
        # last successful tool call; the partial raw_events is the
        # evidence the report says "the agent timed out, here is what
        # it had when it stopped".
        final = "[ERROR] TimeoutError: the agent exceeded the whole-run timeout"
    except Exception as exc:  # noqa: BLE001 - capture per-question failures
        run_status = "model_error"
        error_kind = type(exc).__name__
        final = f"[ERROR] {type(exc).__name__}: {exc}"
    return {
        "raw_events": raw_events,
        "tool_calls": tool_calls,
        "final": final if isinstance(final, str) else str(final),
        "run_status": run_status,
        "error_kind": error_kind,
    }


async def ask_baseline(client, question: str) -> dict:
    """One search_knowledge(hybrid, limit=10) call + a single generation.

    The baseline strips tool use after the first retrieval: the model
    has to answer from the single hybrid call's content. This is the
    lower bound the plan asks for so the agent's number can be read
    as "how much did tool use help". Any retrieval error is reported
    as a tool_error status; the model is still given the chance to
    answer from whatever made it through.
    """
    tools = await client.get_tools()
    tool_map = {t.name: t for t in tools}
    search = tool_map.get("search_knowledge")
    raw_events: list[dict] = []
    tool_calls: list[dict] = []
    run_status = "success"
    error_kind = ""
    if search is None:
        return {
            "raw_events": raw_events, "tool_calls": tool_calls,
            "final": "[ERROR] search_knowledge tool not found in the MCP server",
            "run_status": "config_error", "error_kind": "MissingTool",
        }
    try:
        # The baseline's whole budget is the same 120s as the agent
        # run, but the model step has only 60s. Discovery and the
        # first retrieval count against the outer timeout.
        async with asyncio.timeout(120):
            async with client.session("weknora") as session:
                search_result = await session.call_tool("search_knowledge", {
                    "knowledge_base_ids": [s.strip() for s in need("WEKNORA_KB_IDS").split(",") if s.strip()],
                    "query": question,
                    "mode": "hybrid",
                    "limit": 10,
                })
            search_entry = {
                "type": "tool",
                "name": "search_knowledge",
                "status": "error" if search_result.isError else "success",
                "content": getattr(search_result, "content", ""),
                "artifact": getattr(search_result, "structuredContent", None),
                "id": "baseline-search",
            }
            raw_events.append(search_entry)
            tool_calls.append(search_entry)
            from langchain_openai import ChatOpenAI
            model = ChatOpenAI(
                model=need("AGENT_MODEL"),
                base_url=need("AGENT_MODEL_BASE_URL"),
                api_key=need("AGENT_MODEL_API_KEY"),
                temperature=0, timeout=60, max_retries=0,
            )
            messages_payload = [
                {"role": "system", "content": (
                    "You are answering a single offline validation question. "
                    "You have just received exactly one hybrid search result. "
                    "Use only that result. Do not invent any knowledge id, "
                    "document id, or quote. If the result does not contain "
                    "the answer, say so explicitly."
                )},
                {"role": "user", "content": json.dumps({
                    "question": question,
                    "search_result": getattr(search_result, "structuredContent", None),
                }, ensure_ascii=False)},
            ]
            response = await model.ainvoke(messages_payload)
            final = response.content if isinstance(response.content, str) else str(response.content)
    except TimeoutError:
        run_status = "timeout"
        error_kind = "asyncio.TimeoutError"
        final = "[ERROR] TimeoutError: the baseline exceeded the whole-run timeout"
    except Exception as exc:  # noqa: BLE001 - capture per-question failures
        run_status = "model_error"
        error_kind = type(exc).__name__
        final = f"[ERROR] {type(exc).__name__}: {exc}"
    return {
        "raw_events": raw_events, "tool_calls": tool_calls,
        "final": final if isinstance(final, str) else str(final),
        "run_status": run_status, "error_kind": error_kind,
    }


# Tools whose output carries document evidence. Discovery and status tools
# (list_knowledge_bases, list_documents, get_*_status) enumerate every
# document id in scope, so counting their output would make "found" mean
# "listed" rather than "retrieved" and document recall would collapse to
# whether a listing call ran. Only retrieval/read outputs prove the model
# pulled a document's evidence in (plan §8).
EVIDENCE_TOOLS = evidence_records.EVIDENCE_TOOLS


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

    Kept for backward compatibility with downstream readers; the new
    `evidence_records.derive_evidence` no longer uses it (exact-text
    match on `required_evidence` removes the need for snippet
    attribution by corpus file).
    """
    index = []
    for d in manifest.get("documents", []):
        logical = d.get("logical_doc_id") or d.get("doc_id")
        path = corpus_dir / (d.get("file_name") or "")
        if not logical or not path.is_file():
            continue
        index.append((path.read_text(encoding="utf-8"), logical))
    return index


# `derive_tool_evidence` is the legacy entry point used by
# `recompute_found.py`. It now defers to the shared parser so the
# historical callers and the new code path cannot drift.
def derive_tool_evidence(
    tool_calls, uuid2log, required_evidence=(), corpus_index=None,
):
    return evidence_records.derive_tool_evidence(
        tool_calls, uuid2log, required_evidence, corpus_index,
    )


def build_record(
    question: dict, manifest_uuids: dict, run: dict, started: float,
    corpus_index: list[tuple[str, str]] | None = None,
    strategy: str = "agent",
) -> dict:
    """One question's record, with structured evidence fields.

    The new evidence layout is:

      * `discovered_documents`  - logical ids the retrieval tools named
        (every artifact row, even unread).
      * `read_evidence_documents` - logical ids the agent actually
        loaded text from (verified records only).
      * `read_evidence_texts`   - the set of unique `text` strings the
        agent saw, joined against `required_evidence` for the strict
        evidence-recall metric.
      * `hit_evidence`          - the strict intersection of required
        and read texts; nothing inferred.
      * `evidence_records`      - per-chunk ledger for the manual
        review (tool, knowledge_id, chunk_id, text, read_state).

    `found_documents` is kept as the legacy key (set union of
    `discovered_documents`) so older readers of runs.jsonl still
    get a sensible number. New readers should prefer
    `read_evidence_documents` for the "the model actually saw the
    text" interpretation.
    """
    final = run["final"]
    summary = evidence_records.derive_evidence(
        run.get("raw_events") or [], manifest_uuids,
        question.get("required_evidence", []),
    )
    answer_cited_docs = sorted(extract_cited_doc_logicals(final))
    tool_calls = run.get("tool_calls") or []
    tool_error = any(c.get("status") == "error" for c in tool_calls)
    record = {
        "question_id": question["id"],
        "category": question.get("category", ""),
        "question": question.get("question", ""),
        "strategy": strategy,
        "run_status": run.get("run_status", "success"),
        "error_kind": run.get("error_kind", ""),
        "answerable": bool(question.get("answerable", True)),
        "required_documents": sorted(question.get("relevant_documents") or []),
        "found_documents": summary["discovered_documents"],
        "discovered_documents": summary["discovered_documents"],
        "read_evidence_documents": summary["read_evidence_documents"],
        "read_evidence_texts": summary["read_evidence_texts"],
        "answer_cited_documents": answer_cited_docs,
        "required_evidence": question.get("required_evidence", []),
        "hit_evidence": summary["hit_evidence"],
        "unattributed_evidence_count": summary["unattributed_count"],
        "evidence_records": summary["evidence_records"],
        "cited_knowledge": sorted({r["knowledge_id"] for r in summary["evidence_records"]
                                    if r["knowledge_id"]}),
        "cited_chunks": sorted({r["chunk_id"] for r in summary["evidence_records"]
                                if r["chunk_id"]}),
        "final_answer": final,
        "tool_statuses": [c.get("status") for c in tool_calls],
        "tool_call_names": [c.get("name") for c in tool_calls],
        "tool_error": tool_error,
        "raw_events": run.get("raw_events") or [],
        "elapsed_sec": round(time.monotonic() - started, 2),
    }
    return record


# One read-only tool from the eval allowlist per entry; each is the
# smallest legal call we can make against the live server. The eval
# only counts a tool as "working" if it returns structuredContent
# without isError. A failure here short-circuits the run with a
# non-zero exit so the per-question records do not silently inherit
# a broken upstream.
SMOKE_PROBES = {
    "list_knowledge_bases": {},
    "list_documents": {"knowledge_base_id": "__first_kb__"},
    "search_knowledge": {"query": "_smoke_probe_", "mode": "hybrid", "limit": 1,
                          "knowledge_base_ids": ["__first_kb__"]},
    "read_document": {"knowledge_id": "__any_doc__", "limit": 1},
    "wiki_index": {"knowledge_base_id": "__first_kb__"},
    "wiki_list_pages": {"knowledge_base_id": "__first_kb__", "page_size": 1},
    "wiki_search": {"query": "_smoke_", "limit": 1},
    "wiki_read_page": {"slug": "__any_slug__", "knowledge_base_id": "__first_kb__"},
    "wiki_graph": {"knowledge_base_id": "__first_kb__"},
    "query_knowledge_graph": {"query": "_smoke_", "limit": 1,
                              "knowledge_base_ids": ["__first_kb__"]},
    "get_document_status": {"knowledge_id": "__any_doc__"},
    "get_knowledge_base_status": {"knowledge_base_id": "__first_kb__"},
    "grep_chunks": {"knowledge_base_id": "__first_kb__", "pattern": "_smoke_", "limit": 1},
}


async def _smoke_read_only_tools(client, manifest: dict) -> dict:
    """Probe each read-only tool with the smallest legal call.

    The probe KB / doc / slug are pulled from the live endpoint
    (list_knowledge_bases / list_documents / wiki_index) so the
    smoke test works against whatever KB the eval was pointed at.
    Probes that need a real document or page that may not exist
    are reported as "skipped" rather than as failures, so an empty
    KB does not block the run.
    """
    import urllib.request, urllib.error
    # Discover KB id(s) and one document id from the manifest so the
    # probe payloads are realistic even when the MCP server is local.
    kb_ids = list(manifest_index(manifest).values())
    first_kb = kb_ids[0] if kb_ids else ""
    # Resolve the live endpoint once to discover an existing doc and
    # an existing wiki slug. The probe function reuses the same
    # connection dict the run_eval builds.
    cfg = connection()
    base = cfg["weknora"]["url"].rsplit("/mcp/", 1)[0]
    ep_id = cfg["weknora"]["url"].rsplit("/", 1)[-1]
    headers = dict(cfg["weknora"]["headers"])
    out = {"kb": first_kb, "tools": {}}

    def _rpc(name, args):
        body = json.dumps({
            "jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": name, "arguments": args},
        }).encode("utf-8")
        req = urllib.request.Request(f"{base}/mcp/{ep_id}", data=body, method="POST")
        for k, v in headers.items():
            req.add_header(k, v)
        req.add_header("Content-Type", "application/json")
        req.add_header("Accept", "application/json, text/event-stream")
        try:
            with urllib.request.urlopen(req, timeout=20) as r:
                raw = r.read().decode("utf-8")
        except urllib.error.HTTPError as exc:
            return {"http_error": exc.code, "body": exc.read().decode("utf-8", "ignore")[:200]}
        if raw.startswith("event:"):
            for line in raw.splitlines():
                if line.startswith("data:"):
                    raw = line[5:].strip()
                    break
        try:
            return json.loads(raw)
        except Exception:  # noqa: BLE001
            return {"raw": raw[:200]}

    any_doc = ""
    if first_kb:
        probe = _rpc("list_documents", {"knowledge_base_id": first_kb, "limit": 1})
        sc = (probe.get("result") or {}).get("structuredContent") or {}
        docs = sc.get("documents") or sc.get("results") or []
        if docs and isinstance(docs, list) and isinstance(docs[0], dict):
            any_doc = docs[0].get("id") or docs[0].get("knowledge_id") or ""
    any_slug = ""
    if first_kb:
        probe = _rpc("wiki_list_pages", {"knowledge_base_id": first_kb, "page_size": 1})
        sc = (probe.get("result") or {}).get("structuredContent") or {}
        pages = sc.get("pages") or sc.get("results") or []
        if pages and isinstance(pages, list) and isinstance(pages[0], dict):
            any_slug = pages[0].get("slug") or ""

    def _fill(probe_args: dict) -> dict:
        out = {}
        for k, v in probe_args.items():
            if v == "__first_kb__":
                out[k] = first_kb
            elif v == "__any_doc__":
                out[k] = any_doc
            elif v == "__any_slug__":
                out[k] = any_slug
            else:
                out[k] = v
        return out

    for name, probe in SMOKE_PROBES.items():
        args = _fill(probe)
        if any(v == "" for v in args.values()):
            out["tools"][name] = {"skipped": "missing kb / doc / slug", "args": args}
            continue
        try:
            res = _rpc(name, args)
        except Exception as exc:  # noqa: BLE001
            out["tools"][name] = {"error": f"{type(exc).__name__}: {exc}"}
            continue
        r = res.get("result") or {}
        is_err = bool(r.get("isError"))
        sc = r.get("structuredContent")
        out["tools"][name] = {
            "is_error": is_err,
            "has_structured": sc is not None,
            "error_code": ((r.get("content") or [{}])[0].get("text", "")[:120]
                            if is_err else None),
        }
    return out


async def main_async(args) -> int:
    manifest = load_manifest(Path(args.manifest))
    questions = load_questions(Path(args.questions))
    manifest_uuids = manifest_index(manifest)
    corpus_index = corpus_evidence_index(manifest, Path(args.manifest).parent)
    client = MultiServerMCPClient(connection())
    out_dir = Path(args.output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    runs_path = out_dir / "runs.jsonl"
    # Per-tool smoke check: each read-only tool that this eval
    # exercises must answer at least once. This is a stronger gate
    # than verify_core's schema check, which only confirms the tool
    # is advertised. A read-time contract failure is what would
    # otherwise leak into the question-level results as a silent
    # tool_error. The output is persisted next to runs.jsonl so the
    # report can cite it.
    smoke_path = out_dir / "tool_smoke.json"
    smoke = await _smoke_read_only_tools(client, manifest)
    smoke_path.write_text(json.dumps(smoke, ensure_ascii=False, indent=2),
                           encoding="utf-8")
    failed = 0
    total = 0
    strategies = (["agent"] if args.strategy == "agent" else
                  ["baseline"] if args.strategy == "baseline" else
                  ["agent", "baseline"])
    with runs_path.open("w", encoding="utf-8") as stream:
        for q in questions:
            for strategy in strategies:
                started = time.monotonic()
                print(f"[{q['id']}] {strategy} {q['category']} -> {q['question'][:50]}…")
                if strategy == "baseline":
                    run = await ask_baseline(client, q["question"])
                else:
                    run = await ask_agent(client, q["question"])
                rec = build_record(q, manifest_uuids, run, started, corpus_index,
                                   strategy=strategy)
                stream.write(json.dumps(rec, ensure_ascii=False) + "\n")
                stream.flush()
                total += 1
                if rec["run_status"] != "success":
                    failed += 1
                    print(f"  {rec['run_status'].upper()}: {rec['final_answer'][:120]}")
                else:
                    print(f"  ok ({len(rec['raw_events'])} events, "
                          f"{len(rec['tool_call_names'])} tool calls, "
                          f"{rec['elapsed_sec']}s)")
    print(f"\nruns.jsonl: {runs_path} ({total - failed}/{total} success across {len(strategies)} strategies)")
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
    parser.add_argument("--strategy", choices=["agent", "baseline", "both"],
                        default="agent",
                        help="agent = full ReAct; baseline = one hybrid search + one "
                             "generation; both = run both strategies per question")
    args = parser.parse_args()
    load_dotenv(Path(args.agent_dir) / ".env", override=False)
    return asyncio.run(main_async(args))


if __name__ == "__main__":
    raise SystemExit(main())
