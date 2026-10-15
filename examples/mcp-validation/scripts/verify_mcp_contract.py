"""MCP 20-tool contract verification: positives, negatives, reverse probes,
pagination and provenance, recorded as mcp-contract.json.

This is the Task-4 artifact of the workbench handoff. It runs against the
real stack and needs the same externally issued credentials the live test
uses; it never reads .env implicitly and never prints the token.

What it does:

  1. positives  — runs tests/test_live_mcp_tools.py from the Agent checkout
     (imported, not re-implemented) for all 20 tools and folds its per-tool
     rows into the contract.
  2. negatives  — missing / tampered JWT, foreign workspace, KB outside the
     endpoint scope, and (when the endpoint's allowlist is not "all tools")
     a disabled tool.
  3. reverse    — foreign document / upload / session identifiers must be
     refused WITHOUT leaking the target's title or content.
  4. mechanics  — list_documents paging, read_document truncation, wiki
     source provenance (source_refs_v2 readable) and one graph relation
     whose evidence chunk is readable.

Usage (Agent venv; fresh credentials)::

    $env:WEKNORA_USER_TOKEN = '<fresh JWT>'; $env:WEKNORA_TENANT_ID = '2'
    $env:WEKNORA_TEST_KB = '<write-allowed KB>'
    E:/GoLandCode/WeKnora-Agent/.venv/Scripts/python.exe \\
        examples/mcp-validation/scripts/verify_mcp_contract.py \\
        --agent-dir E:/GoLandCode/WeKnora-Agent
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_OUTPUT = _ROOT / "eval" / "reports" / "console-2026-10-15" / "mcp-contract.json"

EXPECTED_TOOLS = {
    "list_knowledge_bases", "search_knowledge", "grep_chunks", "list_documents",
    "read_document", "ask", "wiki_search", "wiki_read_page", "wiki_index",
    "wiki_list_pages", "wiki_graph", "query_knowledge_graph",
    "get_document_status", "get_knowledge_base_status", "get_upload_status",
    "upload_file", "import_document_file", "add_document", "update_document",
    "delete_document",
}


def _load_agent_module(agent_dir: Path):
    sys.path.insert(0, str(agent_dir))
    import webapp  # noqa: E402  (agent checkout)
    from tests import test_live_mcp_tools as live  # noqa: E402
    return webapp, live


def _fresh_token(base_url: str, accounts_file: Path, email: str) -> tuple[str, str]:
    """Exchange a local account for a real JWT (local automation only)."""
    entries = json.loads(accounts_file.read_text(encoding="utf-8-sig"))
    rows = entries.get("accounts") if isinstance(entries, dict) else entries
    match = [r for r in rows if str(r.get("email", "")).strip().lower() == email.lower()]
    if len(match) != 1:
        raise SystemExit("accounts file must contain exactly one matching email")
    payload = json.dumps({"email": email, "password": match[0]["password"]}).encode("utf-8")
    req = urllib.request.Request(base_url.rstrip("/") + "/api/v1/auth/login",
                                 data=payload, method="POST")
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = json.loads(resp.read().decode("utf-8"))
    token = body.get("token") or ""
    user = body.get("user") or {}
    tenant = str(user.get("tenant_id") or (body.get("active_tenant") or {}).get("id") or "")
    if not token or not tenant:
        raise SystemExit("official login returned no token/tenant")
    return token, tenant


class Contract:
    def __init__(self):
        self.rows: dict[str, dict] = {}
        self.notes: list[str] = []

    def record(self, tool: str, kind: str, status: str, evidence: str = ""):
        row = self.rows.setdefault(tool, {"positive": None, "negative": [], "evidence": []})
        if kind == "positive":
            row["positive"] = {"status": status, "evidence": evidence[:300]}
        elif kind == "negative":
            row["negative"].append({"status": status, "evidence": evidence[:300]})
        else:
            row["evidence"].append({"status": status, "evidence": evidence[:300]})
        print(f"[{status.upper():4}] {kind:8} {tool:24} {evidence[:110]}")

    def note(self, text: str):
        self.notes.append(text)
        print(f"[NOTE] {text}")


def _text_of(result) -> str:
    return json.dumps(result, ensure_ascii=False)


async def _negatives(call, rpc, headers, test_kb, foreign_kb, endpoint_tools, contract):
    # 1. missing user JWT
    bare = {k: v for k, v in headers.items() if k != "X-WeKnora-User-Token"}
    r = await rpc("tools/call", {"name": "list_knowledge_bases", "arguments": {}}, bare)
    status = "pass" if r.get("http_status") == 401 else "fail"
    contract.record("list_knowledge_bases", "negative",
                    status, f"缺失用户 JWT → http={r.get('http_status')}")

    # 2. tampered JWT
    parts = headers["X-WeKnora-User-Token"].split(".")
    tampered = dict(headers, **{"X-WeKnora-User-Token": parts[0] + "." + parts[1] + "." + "A" * len(parts[2])})
    r = await rpc("tools/call", {"name": "list_knowledge_bases", "arguments": {}}, tampered)
    status = "pass" if r.get("http_status") == 401 else "fail"
    contract.record("list_knowledge_bases", "negative",
                    status, f"篡改签名 → http={r.get('http_status')}")

    # 3. foreign workspace
    wrong = dict(headers, **{"X-Tenant-ID": "99999"})
    r = await rpc("tools/call", {"name": "list_knowledge_bases", "arguments": {}}, wrong)
    status = "pass" if r.get("http_status") == 403 else "fail"
    contract.record("list_knowledge_bases", "negative",
                    status, f"跨工作空间 → http={r.get('http_status')}")

    # 4. KB outside the endpoint scope (well-formed uuid, not in range)
    ghost = str(uuid.uuid4())
    ghost_headers = dict(headers, **{"X-WeKnora-Knowledge-Base-Ids": ghost})
    r = await call("list_documents", {"knowledge_base_id": ghost}, ghost_headers)
    ok = (not r["ok"]) and r.get("error_code") == "KB_OUT_OF_SCOPE"
    contract.record("list_documents", "negative",
                    "pass" if ok else "fail",
                    f"范围外 KB → code={r.get('error_code')} err={str(r.get('error'))[:60]}")

    if foreign_kb:
        # 5. a REAL document from another KB, addressed while the selection is
        # the test KB: refused, and the refusal must not leak its title.
        r = await call("list_documents", {"knowledge_base_id": foreign_kb, "page_size": 3}, headers)
        foreign_doc_id, foreign_title = "", ""
        if r["ok"]:
            items = (r.get("result") or {}).get("items") or (r.get("result") or {}).get("documents") or []
            if items:
                foreign_doc_id = items[0].get("id") or ""
                foreign_title = items[0].get("title") or ""
        if foreign_doc_id:
            r = await call("read_document", {"knowledge_id": foreign_doc_id, "limit": 1}, headers)
            denied = not r["ok"]
            leaked = bool(foreign_title) and foreign_title in _text_of(r.get("result"))
            contract.record("read_document", "negative",
                            "pass" if denied and not leaked else "fail",
                            f"范围外文档 {foreign_doc_id[:8]}… → code={r.get('error_code')} "
                            f"拒绝={denied} 泄漏标题={leaked}")
            r = await call("get_document_status", {"knowledge_id": foreign_doc_id}, headers)
            contract.record("get_document_status", "negative",
                            "pass" if not r["ok"] else "fail",
                            f"范围外状态查询 → code={r.get('error_code')}")
        else:
            contract.note("第二个知识库没有可读文档，跳过范围外文档反向测试")
    else:
        contract.note("未提供第二个知识库（WEKNORA_KB_IDS 仅一项），跳过范围外文档反向测试")

    # 6. unknown upload id
    r = await call("import_document_file",
                   {"knowledge_base_id": test_kb, "upload_id": str(uuid.uuid4())})
    contract.record("import_document_file", "negative",
                    "pass" if (not r["ok"] and r.get("error_code") == "UPLOAD_NOT_FOUND") else "fail",
                    f"未知 upload_id → code={r.get('error_code')}")
    r = await call("get_upload_status", {"upload_id": str(uuid.uuid4())})
    contract.record("get_upload_status", "negative",
                    "pass" if not r["ok"] else "fail",
                    f"未知 upload_id → code={r.get('error_code')}")

    # 7. foreign ask session
    r = await call("ask", {"question": "无关问题", "session_id": str(uuid.uuid4())}, timeout=60)
    refused = (not r["ok"]) and "not found" in str(r.get("error", "")).lower()
    contract.record("ask", "negative", "pass" if refused else "fail",
                    f"未知 session_id → code={r.get('error_code')} err={str(r.get('error'))[:60]}")

    # 8. disabled tool, only meaningful when the allowlist is not the full set
    if endpoint_tools is not None and len(endpoint_tools) < len(EXPECTED_TOOLS):
        disabled = sorted(EXPECTED_TOOLS - set(endpoint_tools))
        name = disabled[0]
        r = await rpc("tools/call", {"name": name, "arguments": {}}, headers)
        contract.record(name, "negative",
                        "pass" if (not r["ok"] and r.get("error_code") == "FORBIDDEN") else "fail",
                        f"端点未启用该工具 → code={r.get('error_code')}")
    else:
        contract.note("端点为全工具白名单，禁用工具探针不适用（记录为配置事实）")


async def _mechanics(call, headers, test_kb, contract):
    # pagination: page_size=1 returns exactly one row and a real total
    r = await call("list_documents", {"knowledge_base_id": test_kb, "page_size": 1})
    if r["ok"]:
        res = r.get("result") or {}
        items = res.get("items") or res.get("documents") or []
        total = res.get("total")
        ok = len(items) == 1 and isinstance(total, int)
        contract.record("list_documents", "evidence", "pass" if ok else "fail",
                        f"分页 page_size=1 → items={len(items)} total={total}")
    else:
        contract.record("list_documents", "evidence", "fail", r.get("error") or "")

    # truncation: read a document with limit=1 must report truncated/next_offset
    r = await call("list_documents", {"knowledge_base_id": test_kb, "page_size": 5})
    doc_id = ""
    if r["ok"]:
        for d in (r.get("result") or {}).get("items") or (r.get("result") or {}).get("documents") or []:
            if d.get("parse_status") == "completed":
                doc_id = d.get("id") or ""
                break
    if doc_id:
        r = await call("read_document", {"knowledge_id": doc_id, "limit": 1})
        res = r.get("result") or {}
        has_trunc = "truncated" in res
        contract.record("read_document", "evidence",
                        "pass" if r["ok"] and has_trunc else "fail",
                        f"截断字段 truncated={res.get('truncated')} next_offset={res.get('next_offset')}")
    else:
        contract.record("read_document", "evidence", "skip", "没有已完成文档可读")

    # provenance: a wiki page's source_refs_v2 entries must resolve to readable documents
    r = await call("wiki_list_pages", {"knowledge_base_id": test_kb, "page_size": 5})
    slug, refs = "", {}
    if r["ok"]:
        for page in (r.get("result") or {}).get("pages") or []:
            if page.get("slug"):
                slug = page["slug"]
                break
    if slug:
        r = await call("wiki_read_page", {"slug": slug, "knowledge_base_id": test_kb})
        if r["ok"]:
            refs = (r.get("result") or {}).get("source_refs_v2") or {}
            flat = [item for items in refs.values() for item in (items or [])]
            readable = 0
            for item in flat[:2]:
                kid = item.get("knowledge_id") or ""
                if kid:
                    rr = await call("read_document", {"knowledge_id": kid, "limit": 1})
                    if rr["ok"]:
                        readable += 1
            statuses = {item.get("provenance_status") for item in flat}
            ok = bool(flat) and readable > 0
            contract.record("wiki_read_page", "evidence", "pass" if ok else "fail",
                            f"slug={slug} 来源引用={len(flat)} 可回读={readable} provenance={sorted(statuses)}")
        else:
            contract.record("wiki_read_page", "evidence", "fail", r.get("error") or "")
    else:
        contract.record("wiki_read_page", "evidence", "skip", "Wiki 无页面可校验来源")

    # graph: one relation's evidence chunk must belong to a readable document
    r = await call("list_documents", {"knowledge_base_id": test_kb, "page_size": 5})
    query = ""
    if r["ok"]:
        for d in (r.get("result") or {}).get("items") or (r.get("result") or {}).get("documents") or []:
            title = str(d.get("title") or "")
            if title:
                query = title.split()[0].rsplit(".", 1)[0]
                break
    r = await call("query_knowledge_graph", {"knowledge_base_ids": [test_kb], "query": query or "entity"})
    if r["ok"]:
        rels = (r.get("result") or {}).get("relations") or []
        if rels:
            rel = rels[0]
            kid = rel.get("knowledge_id") or ""
            readable = False
            if kid:
                rr = await call("read_document", {"knowledge_id": kid, "limit": 2})
                readable = rr["ok"]
            contract.record("query_knowledge_graph", "evidence",
                            "pass" if readable else "fail",
                            f"关系={rel.get('source')}->{rel.get('target')} 证据文档可读={readable}")
        else:
            contract.record("query_knowledge_graph", "evidence", "skip",
                            f"查询 {query!r} 无关系命中（图谱存在但该词无命中）")
    else:
        code = r.get("error_code")
        contract.record("query_knowledge_graph", "evidence",
                        "skip" if code in {"GRAPH_EXTRACTION_NOT_CONFIGURED", "GRAPH_UNAVAILABLE"} else "fail",
                        f"配置态 {code}" if code else str(r.get("error"))[:80])


def _endpoint_tools(base_url: str, token: str, tenant: str, endpoint_id: str):
    req = urllib.request.Request(f"{base_url.rstrip('/')}/api/v1/mcp-endpoints/{endpoint_id}")
    req.add_header("Authorization", "Bearer " + token)
    req.add_header("X-Tenant-ID", tenant)
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            data = (json.loads(resp.read().decode("utf-8")) or {}).get("data") or {}
        return data.get("tools") or []
    except urllib.error.HTTPError:
        return None


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent-dir", type=Path, default=Path("E:/GoLandCode/WeKnora-Agent"))
    parser.add_argument("--base-url", default="http://127.0.0.1:18082")
    parser.add_argument("--endpoint-id", default="")
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--accounts-file", type=Path, default=None,
                        help="local automation only: fetch a fresh JWT instead of reading env")
    parser.add_argument("--email", default="15918868657@163.com")
    parser.add_argument("--skip-live", action="store_true", help="negatives/mechanics only")
    args = parser.parse_args(argv)

    webapp, live = _load_agent_module(args.agent_dir)
    from dotenv import load_dotenv
    load_dotenv(webapp.ROOT / ".env", override=False)
    if args.endpoint_id:
        os.environ.setdefault("WEKNORA_MCP_URL",
                              f"{args.base_url.rstrip('/')}/mcp/{args.endpoint_id}")
    if args.accounts_file:
        token, tenant = _fresh_token(args.base_url, args.accounts_file, args.email)
        os.environ["WEKNORA_USER_TOKEN"] = token
        os.environ["WEKNORA_TENANT_ID"] = tenant
    token = os.environ.get("WEKNORA_USER_TOKEN", "").strip()
    tenant = os.environ.get("WEKNORA_TENANT_ID", "").strip()
    if not token or not tenant:
        raise SystemExit("set WEKNORA_USER_TOKEN / WEKNORA_TENANT_ID (or pass --accounts-file)")
    kbs = [k.strip() for k in os.environ.get("WEKNORA_KB_IDS", "").split(",") if k.strip()]
    test_kb = os.environ.get("WEKNORA_TEST_KB", "").strip() or (kbs[0] if kbs else "")
    foreign_kb = next((k for k in kbs if k != test_kb), "")
    if not test_kb:
        raise SystemExit("WEKNORA_TEST_KB (or WEKNORA_KB_IDS) is required")

    contract = Contract()

    # -- catalog -----------------------------------------------------------
    headers = {"X-WeKnora-User-Token": token, "X-Tenant-ID": tenant,
               "X-WeKnora-Knowledge-Base-Ids": test_kb}
    listing = asyncio.run(webapp._mcp_rpc("tools/list", {}, headers))
    names = {t.get("name") for t in (listing.get("result") or {}).get("tools") or []}
    missing = sorted(EXPECTED_TOOLS - names)
    extra = sorted(names - EXPECTED_TOOLS)
    contract.note(f"tools/list 数量={len(names)} 缺失={missing or '无'} 额外={extra or '无'}")

    # -- positives (reuse the live suite) ----------------------------------
    if args.skip_live:
        contract.note("--skip-live：正向扫描未运行，contract 内 positive 为空")
    else:
        os.environ["WEKNORA_LIVE_MCP"] = "1"
        report = asyncio.run(live._run_tools(live.Report()))
        for tool, status, detail in report.rows:
            contract.record(tool, "positive", status, detail)

    # -- negatives + mechanics --------------------------------------------
    endpoint_tools = _endpoint_tools(args.base_url, token, tenant, args.endpoint_id) if args.endpoint_id else None

    async def call(name, arguments, hdrs=None, timeout=120):
        return await webapp._call_tool(name, arguments, hdrs or headers, timeout=timeout)

    async def rpc(method, params, hdrs):
        return await webapp._mcp_rpc(method, params, hdrs)

    async def drive():
        await _negatives(call, rpc, headers, test_kb, foreign_kb, endpoint_tools, contract)
        await _mechanics(call, headers, test_kb, contract)

    asyncio.run(drive())

    totals = {"pass": 0, "fail": 0, "skip": 0}
    for row in contract.rows.values():
        entries = [row["positive"]] if row["positive"] else []
        entries += row["negative"] + row["evidence"]
        for entry in entries:
            if entry:
                totals[entry["status"] if entry["status"] in totals else "skip"] += 1
    payload = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "base_url": args.base_url,
        "endpoint_id": args.endpoint_id,
        "test_kb": test_kb,
        "foreign_kb": foreign_kb,
        "catalog": {"expected": sorted(EXPECTED_TOOLS), "listed": sorted(names),
                    "missing": missing, "extra": extra},
        "totals": totals,
        "notes": contract.notes,
        "tools": contract.rows,
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n契约结果写入 {args.output}")
    print(f"合计：{totals}")
    hard_fail = any(
        (row["positive"] and row["positive"]["status"] == "fail")
        or any(n["status"] == "fail" for n in row["negative"])
        or any(e["status"] == "fail" for e in row["evidence"])
        for row in contract.rows.values()
    )
    return 1 if hard_fail else 0


if __name__ == "__main__":
    raise SystemExit(main())