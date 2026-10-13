#!/usr/bin/env python3
"""Import the demo corpus into a WeKnora knowledge base through the MCP surface.

This is the offline half of the import contract (§7.4 / task 3). It does not
implement any identity or business logic: it reads caller credentials from the
Agent's ``.env`` (or the environment), stages each file through the binary
upload endpoint, then imports it via the MCP ``import_document_file`` tool and
watches ``get_document_status`` until the row is searchable. The manifest is
updated in place with the real WeKnora document ids, atomically, so a report
can always trace a result back to the exact source bytes.

Usage (§11.3)::

    python scripts/import_corpus.py \
        --agent-dir E:/GoLandCode/WeKnora-Agent \
        --manifest eval/corpus/manifest.json \
        --knowledge-base-id <KB-UUID>

Idempotency: the ``Idempotency-Key`` is derived deterministically from the
knowledge-base id and the file SHA-256, so re-running with the same bytes
replays the stored receipt instead of staging a duplicate.
"""
import argparse
import asyncio
import hashlib
import json
import os
import sys
import time
import uuid
from pathlib import Path

import httpx
from dotenv import load_dotenv
from langchain_mcp_adapters.client import MultiServerMCPClient

# Polling cadence fixed by the plan: 2s, 4s, then 8s until the timeout.
_POLL_INTERVALS = [2.0, 4.0]
_POLL_STEADY = 8.0
_DEFAULT_TIMEOUT = 300.0

# Upload is imported; status is queried; both are MCP tools the SDK exposes.
_IMPORT_TOOL = "import_document_file"
_STATUS_TOOL = "get_document_status"
_UPLOAD_STATUS_TOOL = "get_upload_status"

_ID_NAMESPACE = uuid.UUID("8f5a7c1e-2d4b-4a9e-9f6b-3c8d1e0a4b2f")


def need(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise RuntimeError(f"missing environment variable: {name}")
    return value


def headers_with_idempotency(kb_ids: list[str], key: str) -> dict[str, str]:
    return {
        "Authorization": "Bearer " + need("WEKNORA_MCP_TOKEN"),
        "X-WeKnora-User-Token": need("WEKNORA_USER_TOKEN"),
        "X-Tenant-ID": need("WEKNORA_WORKSPACE_ID"),
        "X-WeKnora-Knowledge-Base-Ids": ",".join(kb_ids),
        "Idempotency-Key": key,
    }


def mcp_connection() -> dict:
    kb_ids = [s.strip() for s in need("WEKNORA_KB_IDS").split(",") if s.strip()]
    url = need("WEKNORA_MCP_URL")
    return {
        "weknora": {
            "transport": "streamable_http",
            "url": url,
            "headers": {
                "Authorization": "Bearer " + need("WEKNORA_MCP_TOKEN"),
                "X-WeKnora-User-Token": need("WEKNORA_USER_TOKEN"),
                "X-Tenant-ID": need("WEKNORA_WORKSPACE_ID"),
                "X-WeKnora-Knowledge-Base-Ids": ",".join(kb_ids),
            },
        }
    }


def idempotency_key(kb_id: str, sha256: str) -> str:
    """Deterministic per (KB, bytes) key so a replay returns the same receipt."""
    return str(uuid.uuid5(_ID_NAMESPACE, f"{kb_id}:{sha256}"))


def stage_file(url: str, kb_id: str, file_path: Path, sha256: str) -> dict:
    """Upload one file and verify the receipt matches the source bytes."""
    key = idempotency_key(kb_id, sha256)
    headers = headers_with_idempotency([kb_id], key)
    with file_path.open("rb") as stream:
        with httpx.Client(timeout=120) as client:
            response = client.post(
                url.rstrip("/") + "/uploads",
                headers=headers,
                # Order is deliberate: authorize KB before streaming the file.
                files=[("knowledge_base_id", (None, kb_id)),
                       ("file", (file_path.name, stream, "application/octet-stream"))],
            )
    if response.status_code not in (200, 201):
        raise RuntimeError(
            f"upload failed (HTTP {response.status_code}): {response.text[:500]}")
    body = response.json()
    if not body.get("success"):
        raise RuntimeError(f"upload rejected: {body}")
    receipt = body["data"]
    if receipt.get("sha256") != sha256 or receipt.get("knowledge_base_id") != kb_id:
        raise RuntimeError(f"upload receipt does not match source: {receipt}")
    return receipt


def structured_of(result):
    """Return the structuredContent dict, or {} when the tool returned none."""
    data = getattr(result, "structuredContent", None)
    return data if isinstance(data, dict) else {}


async def import_document(session, kb_id: str, upload_id: str) -> dict:
    result = await session.call_tool(
        _IMPORT_TOOL, {"knowledge_base_id": kb_id, "upload_id": upload_id})
    if getattr(result, "isError", False):
        body = structured_of(result)
        err = body.get("error", {})
        raise RuntimeError(
            f"import failed ({err.get('code', 'UNKNOWN')}): {err.get('message')}")
    return structured_of(result)


async def poll_document_status(session, knowledge_id: str, timeout: float) -> dict:
    """Poll until the document reaches a terminal parse state, or time out."""
    deadline = time.monotonic() + timeout
    attempt = 0
    while True:
        result = await session.call_tool(_STATUS_TOOL, {"knowledge_id": knowledge_id})
        body = structured_of(result)
        status = body.get("status", {})
        parse = status.get("parse_status", "unknown")
        index = status.get("index_status", "unknown")
        if parse in ("completed", "failed", "cancelled", "deleting"):
            return {"parse_status": parse, "index_status": index,
                    "status": status}
        if time.monotonic() >= deadline:
            raise TimeoutError(
                f"document {knowledge_id} did not finish within {timeout}s "
                f"(last state: {parse})")
        # 2s, 4s, then steady 8s.
        delay = _POLL_INTERVALS[attempt] if attempt < len(_POLL_INTERVALS) else _POLL_STEADY
        attempt += 1
        await asyncio.sleep(delay)


def sha256_of(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def load_manifest(path: Path) -> dict:
    with path.open(encoding="utf-8") as stream:
        return json.load(stream)


def atomic_write_manifest(path: Path, manifest: dict) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    with tmp.open("w", encoding="utf-8") as stream:
        json.dump(manifest, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    tmp.replace(path)


async def run(args) -> int:
    manifest_path = Path(args.manifest)
    manifest = load_manifest(manifest_path)
    corpus_dir = manifest_path.parent
    kb_id = args.knowledge_base_id
    url = need("WEKNORA_MCP_URL")
    client = MultiServerMCPClient(mcp_connection())

    documents = manifest.get("documents", [])
    if not documents:
        raise RuntimeError("manifest has no documents")

    failed = 0
    # One session for every MCP call (import + status polling): re-creating a
    # session per call makes langchain-mcp-adapters tear down and rebuild its
    # streamable-http transport each time, which can leave an unhandled task
    # behind and surface as an ExceptionGroup on session exit.
    async with client.session("weknora") as session:
        for doc in documents:
            logical = doc.get("logical_doc_id") or doc.get("doc_id")
            file_name = doc["file_name"]
            source = corpus_dir / file_name
            if not source.exists():
                print(f"[{logical}] SKIP: {file_name} not found in corpus dir")
                failed += 1
                continue
            actual_sha = sha256_of(source)
            expected_sha = doc.get("sha256", "")
            if expected_sha and actual_sha != expected_sha:
                print(f"[{logical}] FAIL: sha256 mismatch (expected {expected_sha[:12]}…, "
                      f"got {actual_sha[:12]}…)")
                failed += 1
                continue

            try:
                receipt = stage_file(url, kb_id, source, actual_sha)
                upload_id = receipt["upload_id"]
                imported = await import_document(session, kb_id, upload_id)
                document = imported.get("document", {})
                knowledge_id = document.get("id")
                if not knowledge_id:
                    raise RuntimeError(f"import returned no document id: {imported}")
                status = await poll_document_status(session, knowledge_id, args.timeout)
                # Back-fill both manifest fields with the same real id (§7.4).
                doc["weknora_document_id"] = knowledge_id
                doc["imported_document_id"] = knowledge_id
                doc["scope"] = kb_id
                doc["imported_at"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
                doc["parse_status"] = status["parse_status"]
                doc["deduplicated"] = imported.get("deduplicated", False)
                print(f"[{logical}] OK: {knowledge_id} (parse={status['parse_status']}"
                      f", dedup={imported.get('deduplicated')})")
            except Exception as exc:  # noqa: BLE001 - one doc failing must not abort the rest
                print(f"[{logical}] FAIL: {type(exc).__name__}: {exc}")
                failed += 1

    # Atomic replace only after the whole run; a failure never half-clears ids.
    atomic_write_manifest(manifest_path, manifest)
    print(f"\nimport finished: {len(documents) - failed}/{len(documents)} succeeded, "
          f"manifest updated at {manifest_path}")
    return 1 if failed else 0


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(
        description="Import the demo corpus into WeKnora through MCP")
    parser.add_argument("--agent-dir", type=Path,
                        default=os.environ.get("WEKNORA_AGENT_DIR", "E:/GoLandCode/WeKnora-Agent"),
                        help="WeKnora-Agent dir whose .env holds the caller identity")
    parser.add_argument("--manifest", type=Path,
                        default=root / "eval/corpus/manifest.json",
                        help="manifest.json to read and back-fill")
    parser.add_argument("--knowledge-base-id", required=True,
                        help="target knowledge base UUID")
    parser.add_argument("--timeout", type=float, default=_DEFAULT_TIMEOUT,
                        help="per-document status poll timeout in seconds")
    args = parser.parse_args()

    agent_dir = Path(args.agent_dir)
    load_dotenv(agent_dir / ".env", override=False)
    try:
        return asyncio.run(run(args))
    except Exception as exc:  # noqa: BLE001 - top-level guard for a clean exit code
        print(f"import aborted: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
