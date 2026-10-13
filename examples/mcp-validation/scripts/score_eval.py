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
import re
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


def build_report(runs_path: Path, review_path: Path, output_path: Path, manifest_path: Path | None) -> None:
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
    manifest_uuids: dict[str, str] = {}
    if manifest_path and manifest_path.exists():
        m = json.loads(manifest_path.read_text(encoding="utf-8"))
        for d in m.get("documents", []):
            logical = d.get("logical_doc_id") or d.get("doc_id")
            uuid_ = d.get("weknora_document_id") or d.get("imported_document_id")
            if logical and uuid_:
                manifest_uuids[logical] = uuid_

    doc_recalls, all_found = [], []
    evidence_recalls = []
    answerable = 0
    unanswerable = 0
    no_answer_honest = 0
    for run in runs:
        required_docs = run.get("required_documents", [])
        # run_eval.py now emits found_documents straight from the tool evidence
        # (cited knowledge UUIDs reverse-resolved through the manifest), and
        # hit_evidence from the tool content. No further re-derivation here.
        found_doc_ids = {d.replace("doc", "") for d in run.get("found_documents", [])}
        required_evidence = run.get("required_evidence") or []
        hit_evidences = run.get("hit_evidence") or []
        run["_found_doc_ids"] = sorted(found_doc_ids)
        run["_hit_evidence"] = hit_evidences
        if not required_docs:
            run["_doc_metrics"] = document_metrics([], [])
            run["_answerable"] = False
            unanswerable += 1
            # Honest handling: no required_evidence fragment quoted as truth,
            # AND the run did not surface a tool/service error (an error is
            # "could not answer", which the plan distinguishes from "no
            # evidence exists"). Fabrication is judged by the human review's
            # refusal_fabricated_content field, not guessed here.
            if not hit_evidences and not run.get("tool_error"):
                no_answer_honest += 1
            continue
        answerable += 1
        run["_answerable"] = True
        dm = document_metrics({d.replace("doc", "") for d in required_docs}, found_doc_ids)
        em = evidence_metrics(required_evidence, hit_evidences)
        run["_doc_metrics"] = dm
        run["_evidence_metrics"] = em
        if dm["recall"] is not None:
            doc_recalls.append(dm["recall"])
        if dm["all_found"] is not None:
            all_found.append(dm["all_found"])
        if em["recall"] is not None:
            evidence_recalls.append(em["recall"])

    # Human-review metrics: only count questions the reviewer actually marked
    # reviewed=true. Unreviewed questions contribute nothing (never 0).
    manual = review.get("questions", {}) if isinstance(review, dict) else {}
    fact_acc, fact_comp, cit_sup = [], [], []
    reviewed = 0
    for qid, entry in manual.items():
        hr = entry.get("human_review") or {}
        if not hr.get("reviewed"):
            continue
        reviewed += 1
        if hr.get("verifiable_claims"):
            fact_acc.append(hr.get("correct_claims", 0) / hr["verifiable_claims"])
        if hr.get("necessary_facts_total"):
            fact_comp.append(hr.get("covered_facts", 0) / hr["necessary_facts_total"])
        if hr.get("total_cited_claims"):
            cit_sup.append(hr.get("supported_cited_claims", 0) / hr["total_cited_claims"])

    lines = ["# WeKnora MCP 验证评估报告", ""]
    lines.append(f"总题数：{len(runs)}（可回答 {answerable}，无答案 {unanswerable}）")
    lines.append(f"文档 recall（宏平均）：{macro_average(doc_recalls)}")
    lines.append(f"文档找齐率：{sum(all_found)}/{len(all_found)}")
    lines.append(f"必需证据 recall（宏平均，工具证据子串匹配）：{macro_average(evidence_recalls)}")
    lines.append(f"无答案诚实处理（无编造证据 + 无工具错误）：{no_answer_honest}/{unanswerable}")
    lines.append("")
    lines.append("## 人工复核指标")
    lines.append(f"已复核题数：{reviewed}/{len(manual)}")
    lines.append(f"事实准确率（宏平均）：{macro_average(fact_acc)}")
    lines.append(f"事实完整率（宏平均）：{macro_average(fact_comp)}")
    lines.append(f"引用支持率（宏平均）：{macro_average(cit_sup)}")
    if reviewed == 0:
        lines.append("")
        lines.append("> 尚未有人工复核，事实准确率 / 完整率 / 引用支持率为 not_evaluated。")
    output_path.write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description="Score an offline eval run")
    parser.add_argument("--runs", type=Path,
                        default=root / "eval/reports/runs.jsonl")
    parser.add_argument("--manual-review", type=Path,
                        default=root / "eval/reports/manual-review.json")
    parser.add_argument("--manifest", type=Path,
                        default=root / "eval/corpus/manifest.json",
                        help="manifest used to reverse-resolve cited WeKnora UUIDs back to logical doc ids")
    parser.add_argument("--output", type=Path,
                        default=root / "eval/reports/report.md")
    args = parser.parse_args()
    build_report(args.runs, args.manual_review, args.output, args.manifest)
    print(f"report written to {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
