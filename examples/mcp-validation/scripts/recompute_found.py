#!/usr/bin/env python3
"""Recompute found_documents for the runs.jsonl the previous run produced.

This avoids paying the LLM cost to rerun 12 questions when only the
citation-extraction logic changed. The final answer text is the source of
truth; found_documents is now read straight from it via the same regex
used by run_eval.build_record.
"""
import argparse
import json
import re
import sys
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(_ROOT / "scripts"))
import run_eval  # noqa: E402


def recompute(runs_path: Path) -> int:
    n = 0
    with runs_path.open(encoding="utf-8") as src, \
         runs_path.with_suffix(".jsonl.tmp").open("w", encoding="utf-8") as dst:
        for line in src:
            if not line.strip():
                continue
            rec = json.loads(line)
            final = rec.get("final_answer", "")
            cited = run_eval.find_cited_ids(final)
            cited_docs = run_eval.extract_cited_doc_logicals(final) | cited["doc_logical"]
            rec["found_documents"] = sorted(cited_docs)
            # required_evidence is also re-derived so score_eval matches the
            # original plan definition.
            rec["hit_evidence"] = [e for e in rec.get("required_evidence", [])
                                   if e and e[:20] in final]
            dst.write(json.dumps(rec, ensure_ascii=False) + "\n")
            n += 1
    runs_path.with_suffix(".jsonl.tmp").replace(runs_path)
    return n


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=Path,
                        default=_ROOT / "eval/reports/runs.jsonl")
    args = parser.parse_args()
    n = recompute(args.runs)
    print(f"recomputed {n} rows in {args.runs}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
