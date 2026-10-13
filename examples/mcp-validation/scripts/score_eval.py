#!/usr/bin/env python3
"""Offline scoring for the WeKnora MCP validation run.

Pure functions only: document recall / all-found, evidence recall, fact
accuracy/completeness and citation-support metrics. The plan fixes these
definitions (§8); the input is run_eval.py's runs.jsonl plus the manual
review file, never the WeKnora service directly. Every metric returns its
numerator and denominator so a report can show the actual counts rather than
a naked percentage.

Usage (§11.3)::

    python scripts/score_eval.py \
        --runs eval/reports/runs.jsonl \
        --manual-review eval/reports/manual-review.json \
        --output eval/reports/report.md
"""
import argparse
import json
from pathlib import Path


def document_metrics(required, found):
    """Recall and all-found over one question's relevant documents.

    An empty required set (an unanswerable question with no relevant docs)
    yields None for both, never a fake 100%. See the plan's no-answer rule.
    """
    required, found = set(required), set(found)
    matched = len(required & found)
    return {"matched": matched, "required": len(required),
            "recall": matched / len(required) if required else None,
            "all_found": required <= found if required else None}


def evidence_metrics(required, read):
    """Recall over evidence items actually read (not merely IDs discovered)."""
    required, read = set(required), set(read)
    matched = len(required & read)
    return {"matched": matched, "required": len(required),
            "recall": matched / len(required) if required else None}


def macro_average(recalls):
    values = [r for r in recalls if r is not None]
    return sum(values) / len(values) if values else None


def fact_metrics(correct, total_verifiable):
    """Fact accuracy (manual): correct / verifiable facts output."""
    if total_verifiable == 0:
        return {"correct": correct, "total": total_verifiable, "accuracy": None}
    return {"correct": correct, "total": total_verifiable,
            "accuracy": correct / total_verifiable}


def completeness_metrics(covered, required):
    """Fact completeness (manual): covered / required facts."""
    if required == 0:
        return {"covered": covered, "required": required, "completeness": None}
    return {"covered": covered, "required": required,
            "completeness": covered / required}


def citation_support_metrics(supported, total):
    """Citation support: citations actually backed by the source text."""
    if total == 0:
        return {"supported": supported, "total": total, "support_rate": None}
    return {"supported": supported, "total": total, "support_rate": supported / total}


def build_report(runs_path: Path, review_path: Path, output_path: Path) -> None:
    runs = []
    if runs_path.exists():
        with runs_path.open(encoding="utf-8") as stream:
            for line in stream:
                line = line.strip()
                if line:
                    runs.append(json.loads(line))
    review = {}
    if review_path.exists():
        with review_path.open(encoding="utf-8") as stream:
            review = json.load(stream)

    doc_recalls, all_found = [], []
    evidence_recalls = []
    answerable = 0
    for run in runs:
        question = run.get("question_id", "?")
        required_docs = run.get("required_documents", [])
        found_docs = run.get("found_documents", [])
        required_evidence = run.get("required_evidence", [])
        read_evidence = run.get("read_evidence", [])
        # Unanswerable questions (no required docs) are counted separately.
        if not required_docs:
            run["_doc_metrics"] = document_metrics([], [])
            run["_answerable"] = False
            continue
        answerable += 1
        run["_answerable"] = True
        dm = document_metrics(required_docs, found_docs)
        em = evidence_metrics(required_evidence, read_evidence)
        run["_doc_metrics"] = dm
        run["_evidence_metrics"] = em
        if dm["recall"] is not None:
            doc_recalls.append(dm["recall"])
        if dm["all_found"] is not None:
            all_found.append(dm["all_found"])
        if em["recall"] is not None:
            evidence_recalls.append(em["recall"])

    lines = ["# WeKnora MCP 验证评估报告", ""]
    lines.append(f"可回答题数：{answerable}")
    lines.append(f"文档 recall（宏平均）：{macro_average(doc_recalls)}")
    lines.append(f"文档找齐率：{sum(all_found)}/{len(all_found)}")
    lines.append(f"必需证据 recall（宏平均）：{macro_average(evidence_recalls)}")
    # Manual review rows are keyed by question id; the plan requires a human to
    # fill fact/citation counts before accuracy is evaluated.
    manual = review.get("questions", {}) if isinstance(review, dict) else {}
    lines.append(f"人工复核题数：{len(manual)}")
    lines.append("")
    lines.append("> 事实准确率 / 完整率 / 引用支持率需人工复核完成后才能评出；")
    lines.append("> 未复核时保持 not_evaluated。")
    output_path.write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description="Score an offline eval run")
    parser.add_argument("--runs", type=Path,
                        default=root / "eval/reports/runs.jsonl")
    parser.add_argument("--manual-review", type=Path,
                        default=root / "eval/reports/manual-review.json")
    parser.add_argument("--output", type=Path,
                        default=root / "eval/reports/report.md")
    args = parser.parse_args()
    build_report(args.runs, args.manual_review, args.output)
    print(f"report written to {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
