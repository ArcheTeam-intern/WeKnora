#!/usr/bin/env python3
"""Native SDK self-check against the real WeKnora MCP endpoint.

Verifies the endpoint exposes the 13 read-only tools and that a retrieval,
an original-text read, a wiki read and a knowledge-graph query each return
without an MCP error. This is the "does the service actually answer through
MCP" gate before the real-model Agent run (task 7); it records tool schemas
and results under the output dir but never the connection headers.

Usage (§11.3)::

    python scripts/mcp_smoke.py \
        --agent-dir E:/GoLandCode/WeKnora-Agent \
        --output-dir eval/reports/mcp-smoke
"""
import argparse
import asyncio
import json
import os
import sys
from pathlib import Path

from dotenv import load_dotenv
from langchain_mcp_adapters.client import MultiServerMCPClient

READ_ONLY_TOOLS = {
    "list_knowledge_bases", "list_documents", "search_knowledge", "grep_chunks",
    "read_document", "wiki_index", "wiki_search", "wiki_read_page",
    "wiki_list_pages", "wiki_graph", "query_knowledge_graph",
    "get_document_status", "get_knowledge_base_status",
}


def need(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise ValueError(f"missing environment variable: {name}")
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


async def verify(output_dir: Path) -> dict:
    kb_ids = [s.strip() for s in need("WEKNORA_KB_IDS").split(",") if s.strip()]
    client = MultiServerMCPClient(connection())
    # Calls that only need the KB scope, not a pre-known document/wiki id.
    calls = [
        ("list_knowledge_bases", {}),
        ("search_knowledge", {"query": "云雀平台", "mode": "hybrid",
                              "knowledge_base_ids": kb_ids}),
    ]
    # Optional by-id probes, driven by env so the smoke test stays generic.
    test_knowledge_id = os.environ.get("TEST_KNOWLEDGE_ID", "").strip()
    test_wiki_slug = os.environ.get("TEST_WIKI_SLUG", "").strip()
    if test_knowledge_id:
        calls.append(("read_document", {"knowledge_id": test_knowledge_id, "limit": 5}))
    if test_wiki_slug:
        calls.append(("wiki_read_page", {"slug": test_wiki_slug,
                                         "knowledge_base_id": kb_ids[0]}))
    calls.append(("query_knowledge_graph", {"knowledge_base_ids": kb_ids,
                                            "query": "项目负责人"}))

    report = {"tools": {}, "calls": []}
    async with asyncio.timeout(120):
        async with client.session("weknora") as session:
            listed = await session.list_tools()
            available = {tool.name for tool in listed.tools}
            missing = READ_ONLY_TOOLS - available
            report["available_tools"] = sorted(available)
            report["missing_required_tools"] = sorted(missing)
            # Record schemas only; never the connection headers.
            report["tools"] = {tool.name: tool.inputSchema for tool in listed.tools}
            for name, args in calls:
                result = await session.call_tool(name, args)
                entry = {
                    "tool": name,
                    "is_error": bool(getattr(result, "isError", False)),
                }
                if not getattr(result, "isError", False):
                    entry["result"] = result.model_dump(mode="json", by_alias=True)
                else:
                    entry["structured"] = getattr(result, "structuredContent", None)
                report["calls"].append(entry)
                if entry["is_error"] and name in READ_ONLY_TOOLS:
                    report["failed_required_call"] = name
    return report


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description="Native SDK self-check for WeKnora MCP")
    parser.add_argument("--agent-dir", type=Path,
                        default=os.environ.get("WEKNORA_AGENT_DIR", "E:/GoLandCode/WeKnora-Agent"),
                        help="WeKnora-Agent dir whose .env holds the caller identity")
    parser.add_argument("--output-dir", type=Path,
                        default=root / "eval/reports/mcp-smoke",
                        help="directory for schemas + call results (no headers)")
    args = parser.parse_args()

    load_dotenv(Path(args.agent_dir) / ".env", override=False)
    args.output_dir.mkdir(parents=True, exist_ok=True)
    try:
        report = asyncio.run(verify(args.output_dir))
    except Exception as exc:  # noqa: BLE001 - top-level guard for a clean exit code
        print(f"smoke check failed: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 2

    out = args.output_dir / "smoke.json"
    out.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")

    missing = report.get("missing_required_tools", [])
    failed = report.get("failed_required_call")
    print(f"tools listed: {len(report.get('available_tools', []))}")
    if missing:
        print(f"MISSING required tools: {', '.join(missing)}")
    if failed:
        print(f"FAILED required call: {failed}")
    print(f"report: {out}")
    if missing or failed:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
