#!/usr/bin/env python3
"""Recompute the tool-evidence fields of an existing runs.jsonl.

Pays no LLM cost: the raw tool traces are already stored per record, so a
change to the citation-extraction logic is re-applied offline. The source of
truth is the retrieval tools' output (search/grep/read/ask/graph/wiki) —
discovery and status tools are excluded because they list every document id
in scope, which would make "found" mean "listed" rather than "retrieved".
The derivation lives in run_eval.derive_tool_evidence so both paths stay
identical.
"""
import argparse
import json
import sys
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(_ROOT / "scripts"))
import run_eval  # noqa: E402


def tool_calls_from_events(rec: dict) -> list[dict]:
    out = []
    for ev in rec.get("raw_events", []):
        if ev.get("type") != "tool":
            continue
        out.append({
            "name": ev.get("name", ""),
            "content": ev.get("content", ""),
            "status": ev.get("status", ""),
            "artifact": ev.get("artifact"),
        })
    return out


def recompute(runs_path: Path, manifest_path: Path) -> int:
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    uuid2log = {}
    for d in manifest.get("documents", []):
        logical = d.get("logical_doc_id") or d.get("doc_id")
        uuid_ = d.get("weknora_document_id") or d.get("imported_document_id")
        if logical and uuid_:
            uuid2log[uuid_] = logical
    corpus_index = run_eval.corpus_evidence_index(manifest, manifest_path.parent)
    n = 0
    with runs_path.open(encoding="utf-8") as src, \
         runs_path.with_suffix(".jsonl.tmp").open("w", encoding="utf-8") as dst:
        for line in src:
            if not line.strip():
                continue
            rec = json.loads(line)
            derived = run_eval.derive_tool_evidence(
                tool_calls_from_events(rec), uuid2log,
                rec.get("required_evidence") or [], corpus_index)
            rec["found_documents"] = derived["found_documents"]
            rec["cited_knowledge"] = derived["cited_knowledge"]
            rec["cited_chunks"] = derived["cited_chunks"]
            rec["hit_evidence"] = derived["hit_evidence"]
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
