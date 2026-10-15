"""End-to-end verification of the WeKnora MCP core contract.

P1-1 of the handover plan asks for an automated walk over the 13
read-only tools using the raw MCP `tools/list` and `tools/call` RPCs
(not the LLM's tool-choice layer), so a passing run proves the
protocol end-to-end rather than just the agent's interpretation of
it. The script also walks the permission boundaries and CORS headers
the plan calls out, and records what it found to a JSON file under
the current-execution reports directory.

The script is intentionally small and explicit: each assertion is its
own function with a printable status line, so a failure pins exactly
which contract piece is missing. It does not invoke an LLM; the
permission and schema checks must pass without the model in the loop.

Usage::

    E:/GoLandCode/WeKnora-Agent/.venv/Scripts/python.exe \\
        scripts/verify_core.py \\
        --agent-dir E:/GoLandCode/WeKnora-Agent \\
        --config deploy/acceptance.local.json \\
        --output-dir eval/reports/correction-current
"""
from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.request
import urllib.error
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(_ROOT / "scripts"))

# Read-only tools the agent's tool filter is expected to expose.
EXPECTED_READ_ONLY_TOOLS = {
    "list_knowledge_bases",
    "list_documents",
    "search_knowledge",
    "grep_chunks",
    "read_document",
    "wiki_index",
    "wiki_search",
    "wiki_read_page",
    "wiki_list_pages",
    "wiki_graph",
    "query_knowledge_graph",
    "get_document_status",
    "get_knowledge_base_status",
}


# --- low-level MCP RPC -----------------------------------------------------

def _rpc_list_tools(base_url: str, endpoint_id: str, headers: dict) -> dict:
    """Send a `tools/list` JSON-RPC and return the raw response.

    Uses the streamable_http transport by issuing a POST to
    `/mcp/<endpoint>` with an `accept: application/json, text/event-stream`
    header; the server answers as JSON when the call has no body that
    requires streaming.
    """
    body = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {},
    }).encode("utf-8")
    req = urllib.request.Request(
        base_url.rstrip("/") + "/mcp/" + endpoint_id,
        data=body, method="POST",
    )
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    for k, v in headers.items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=20) as resp:
        text = resp.read().decode("utf-8")
        # Streamable HTTP may wrap in SSE; unwrap the first data: line.
        if text.startswith("event:"):
            for line in text.splitlines():
                if line.startswith("data:"):
                    text = line[len("data:"):].strip()
                    break
        return json.loads(text)


def _rpc_call_tool(
    base_url: str, endpoint_id: str, headers: dict,
    name: str, arguments: dict, call_id: int = 2,
) -> dict:
    body = json.dumps({
        "jsonrpc": "2.0", "id": call_id, "method": "tools/call",
        "params": {"name": name, "arguments": arguments},
    }).encode("utf-8")
    req = urllib.request.Request(
        base_url.rstrip("/") + "/mcp/" + endpoint_id,
        data=body, method="POST",
    )
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            text = resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        return {"_http_error": exc.code, "_body": exc.read().decode("utf-8", "ignore")}
    if text.startswith("event:"):
        for line in text.splitlines():
            if line.startswith("data:"):
                text = line[len("data:"):].strip()
                break
    return json.loads(text)


def _options_cors(url: str) -> dict:
    """Send an OPTIONS preflight and return the response headers."""
    req = urllib.request.Request(url, method="OPTIONS")
    req.add_header("Origin", "https://example.com")
    req.add_header("Access-Control-Request-Method", "POST")
    req.add_header("Access-Control-Request-Headers",
                   "authorization,x-weknora-user-token,x-tenant-id,"
                   "x-weknora-knowledge-base-ids,idempotency-key,content-type")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return {k.lower(): v for k, v in resp.headers.items()}
    except urllib.error.HTTPError as exc:
        return {k.lower(): v for k, v in exc.headers.items()} | {"_status": exc.code}


# --- assertions ------------------------------------------------------------

def _check_tool_schema(tool: dict) -> list[str]:
    """A tool descriptor must have name + description + an inputSchema."""
    issues: list[str] = []
    if not tool.get("name"):
        issues.append("missing name")
    if not tool.get("description"):
        issues.append("missing description")
    schema = tool.get("inputSchema") or {}
    if not isinstance(schema, dict):
        issues.append("inputSchema is not an object")
    elif schema.get("type") != "object":
        issues.append(f"inputSchema.type is {schema.get('type')!r}, expected 'object'")
    return issues


def check_tools_list(base_url: str, endpoint_id: str, headers: dict) -> dict:
    rpc = _rpc_list_tools(base_url, endpoint_id, headers)
    result = rpc.get("result") or {}
    tools = result.get("tools") or []
    names = sorted(t.get("name", "") for t in tools)
    missing = sorted(EXPECTED_READ_ONLY_TOOLS - set(names))
    schema_issues = {t.get("name", "<unnamed>"): _check_tool_schema(t) for t in tools
                     if _check_tool_schema(t)}
    return {
        "rpc_id": rpc.get("id"),
        "discovered_count": len(tools),
        "discovered_names": names,
        "expected_missing": missing,
        "schema_issues": schema_issues,
    }


def check_call_search(base_url: str, endpoint_id: str, headers: dict, kb_id: str) -> dict:
    """search_knowledge with the four required headers must succeed."""
    r = _rpc_call_tool(base_url, endpoint_id, headers, "search_knowledge", {
        "knowledge_base_ids": [kb_id], "query": "test", "mode": "hybrid", "limit": 3,
    })
    res = r.get("result") or {}
    return {
        "is_error": bool(res.get("isError")),
        "has_structured": bool(res.get("structuredContent")),
        "http_error": r.get("_http_error"),
    }


def check_call_without_user_token(base_url: str, endpoint_id: str, headers: dict, kb_id: str) -> dict:
    """A caller that omits `X-WeKnora-User-Token` must be refused cleanly,
    not silently served by the endpoint token alone. The plan requires
    four headers, not one. The endpoint token is a credential for the
    MCP endpoint, not a substitute for the user identity.
    """
    dropped = {k: v for k, v in headers.items()
               if k.lower() != "x-weknora-user-token"}
    r = _rpc_call_tool(base_url, endpoint_id, dropped, "list_knowledge_bases", {})
    res = r.get("result") or {}
    return {
        "is_error": bool(res.get("isError")),
        "http_error": r.get("_http_error"),
    }


def check_call_out_of_scope_kb(base_url: str, endpoint_id: str, headers: dict, valid_kb: str) -> dict:
    """A knowledge base id outside the request selection must be refused,
    not silently truncated to a different kb. The header is
    `X-WeKnora-Knowledge-Base-Ids`: a single string here means the
    request is restricted to it, so a different id must error.
    """
    r = _rpc_call_tool(base_url, endpoint_id, headers, "list_documents", {
        "knowledge_base_ids": ["ffffffff-ffff-ffff-ffff-ffffffffffff"],
    })
    res = r.get("result") or {}
    return {
        "is_error": bool(res.get("isError")),
        "http_error": r.get("_http_error"),
    }


def check_cors(base_url: str) -> dict:
    """CORS preflight must allow the four MCP headers and Idempotency-Key.

    The plan requires OPTIONS to admit every header a real client might
    send; the test exercises the exact set the WeKnora UI and the
    LangChain MCP adapter use.
    """
    url = base_url.rstrip("/") + "/mcp/" + "anything"
    headers = _options_cors(url)
    allow = headers.get("access-control-allow-headers", "")
    required = [
        "authorization",
        "x-weknora-user-token",
        "x-tenant-id",
        "x-weknora-knowledge-base-ids",
        "idempotency-key",
    ]
    missing = [h for h in required if h not in allow.lower()]
    return {
        "status": headers.get("_status"),
        "allow_headers": allow,
        "missing": missing,
    }


def check_health(base_url: str) -> dict:
    req = urllib.request.Request(base_url.rstrip("/") + "/health", method="GET")
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return {"status": resp.status, "body": resp.read().decode("utf-8")}
    except urllib.error.HTTPError as exc:
        return {"status": exc.code, "body": exc.read().decode("utf-8", "ignore")}


# --- main ------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(description="End-to-end MCP core verification")
    parser.add_argument("--agent-dir", type=Path,
                        default=Path("E:/GoLandCode/WeKnora-Agent"))
    parser.add_argument("--config", type=Path,
                        default=_ROOT / "deploy/acceptance.local.json")
    parser.add_argument("--output-dir", type=Path,
                        default=_ROOT / "eval/reports/correction-current")
    args = parser.parse_args()

    from dotenv import dotenv_values
    env = dotenv_values(args.agent_dir / ".env")
    cfg = json.loads(args.config.read_text(encoding="utf-8"))
    base_url = cfg["base_url"]
    endpoint_id = cfg["endpoint_id"]
    workspace_id = cfg["workspace_id"]
    headers = {
        "Authorization": "Bearer " + cfg["endpoint_token"],
        "X-WeKnora-User-Token": env.get("WEKNORA_USER_TOKEN", ""),
        "X-Tenant-ID": str(workspace_id),
        "X-WeKnora-Knowledge-Base-Ids": cfg["kb_id"],
    }

    args.output_dir.mkdir(parents=True, exist_ok=True)

    report = {
        "started_at": int(time.time()),
        "config": {
            "base_url": base_url,
            "endpoint_id": endpoint_id,
            "kb_id": cfg["kb_id"],
        },
    }
    report["health"] = check_health(base_url)
    report["tools_list"] = check_tools_list(base_url, endpoint_id, headers)
    report["call_search"] = check_call_search(base_url, endpoint_id, headers, cfg["kb_id"])
    report["call_without_user_token"] = check_call_without_user_token(
        base_url, endpoint_id, headers, cfg["kb_id"])
    report["call_out_of_scope_kb"] = check_call_out_of_scope_kb(
        base_url, endpoint_id, headers, cfg["kb_id"])
    report["cors"] = check_cors(base_url)

    out_path = args.output_dir / "verify_core.json"
    out_path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"verify_core report written to {out_path}")
    # Summarise: tools missing, schema issues, CORS missing, call errors.
    summary = {
        "missing_tools": report["tools_list"]["expected_missing"],
        "schema_issues": [k for k, v in report["tools_list"]["schema_issues"].items() if v],
        "cors_missing": report["cors"]["missing"],
        "search_error": report["call_search"]["is_error"] or report["call_search"]["http_error"],
        "no_user_token_passes": not report["call_without_user_token"]["is_error"]
                                and not report["call_without_user_token"]["http_error"],
        "out_of_scope_passes": not report["call_out_of_scope_kb"]["is_error"]
                                and not report["call_out_of_scope_kb"]["http_error"],
    }
    summary_path = args.output_dir / "verify_core_summary.json"
    summary_path.write_text(json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    # Exit 0 if all checks pass; non-zero so the caller can wire it into CI.
    if (summary["missing_tools"] or summary["schema_issues"] or
            summary["cors_missing"] or summary["search_error"] or
            summary["no_user_token_passes"] or summary["out_of_scope_passes"]):
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
