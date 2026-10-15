#!/usr/bin/env python3
"""Recompute the tool-evidence fields of an existing runs.jsonl.

Pays no LLM cost: the raw tool traces are already stored per record, so a
change to the citation-extraction logic is re-applied offline. The source of
truth is the retrieval tools' output (search/grep/read/ask/graph/wiki) —
discovery and status tools are excluded because they list every document id
in scope, which would make "found" mean "listed" rather than "retrieved".

The derivation lives in `evidence_records.derive_evidence` so the live
`run_eval` and the offline recompute share one parser. The legacy
`run_eval.derive_tool_evidence` shim is kept for callers that have not
migrated; the recompute script uses the new shared function directly
because the schema of the persisted record was extended with
`discovered_documents` / `read_evidence_documents` / `evidence_records`.
"""
import argparse
import json
import sys
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(_ROOT / "scripts"))
import evidence_records  # noqa: E402
import run_eval  # noqa: E402 - kept so the legacy call shape still works


def _manifest_index(manifest: dict) -> dict[str, str]:
    """Return a logical_doc_id -> real WeKnora knowledge_id index.

    `evidence_records.derive_evidence` takes the same direction as the
    manifest itself: it joins each parsed chunk's `knowledge_id` against
    the manifest's `weknora_document_id` to attribute the chunk to a
    `logical_doc_id`. Both directions are kept in the index because the
    new parser also checks the reverse mapping in case a future
    manifest writes `knowledge_id` as the key.
    """
    out: dict[str, str] = {}
    for d in manifest.get("documents", []):
        logical = d.get("logical_doc_id") or d.get("doc_id")
        uuid_ = d.get("weknora_document_id") or d.get("imported_document_id")
        if logical and uuid_:
            out[logical] = uuid_
            out.setdefault(uuid_, logical)
    return out


def recompute(runs_path: Path, manifest_path: Path) -> int:
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    index = _manifest_index(manifest)
    n = 0
    with runs_path.open(encoding="utf-8") as src, \
         runs_path.with_suffix(".jsonl.tmp").open("w", encoding="utf-8") as dst:
        for line in src:
            if not line.strip():
                continue
            rec = json.loads(line)
            summary = evidence_records.derive_evidence(
                rec.get("raw_events") or [], index,
                rec.get("required_evidence") or [],
            )
            rec["found_documents"] = summary["discovered_documents"]
            rec["discovered_documents"] = summary["discovered_documents"]
            rec["read_evidence_documents"] = summary["read_evidence_documents"]
            rec["read_evidence_texts"] = summary["read_evidence_texts"]
            rec["hit_evidence"] = summary["hit_evidence"]
            rec["unattributed_evidence_count"] = summary["unattributed_count"]
            rec["evidence_records"] = summary["evidence_records"]
            rec["cited_knowledge"] = sorted(
                r["knowledge_id"] for r in summary["evidence_records"]
                if r["knowledge_id"]
            )
            rec["cited_chunks"] = sorted(
                r["chunk_id"] for r in summary["evidence_records"]
                if r["chunk_id"]
            )
            dst.write(json.dumps(rec, ensure_ascii=False) + "\n")
            n += 1
    runs_path.with_suffix(".jsonl.tmp").replace(runs_path)
    return n


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=Path,
                        default=_ROOT / "eval/reports/runs.jsonl")
    parser.add_argument("--manifest", type=Path,
                        default=_ROOT / "eval/corpus/manifest.json")
    args = parser.parse_args()
    n = recompute(args.runs, args.manifest)
    print(f"recomputed {n} rows in {args.runs}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
