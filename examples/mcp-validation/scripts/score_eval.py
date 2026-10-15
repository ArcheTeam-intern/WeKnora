#!/usr/bin/env python3
"""Offline scoring for the WeKnora MCP validation run.

Pure functions only: document recall / all-found, evidence recall, fact
accuracy/completeness and citation-support metrics. The plan fixes these
definitions (§8); the input is run_eval.py's runs.jsonl plus the manual
review file, never the WeKnora service directly. Every metric returns its
numerator and denominator so a report can show the actual counts rather than
a naked percentage.

The runner records (one per (question, strategy) pair) are grouped by
strategy so the report can show agent vs baseline side by side. A run
that did not finish with `run_status == "success"` contributes its
`run_status` to the report's run-classification section, but never to
the recall numerators: a timeout / model_error / config_error result
is "could not answer", not "answered wrongly", and the plan explicitly
keeps those distinct.

The previous "no-answer auto-pass" rule is removed: an unanswerable
question no longer passes simply because the run did not surface a
tool error and did not produce hit_evidence. The honest handling
signal now comes from the manual review's `refusal_supported` /
`fabricated_content` / `citation_supported` fields. The auto side
just reports the raw counts so the manual side can see what it has
to grade.

Usage (§11.3)::

    python scripts/score_eval.py \
        --runs eval/reports/correction-current/runs.jsonl \
        --manual-review eval/reports/correction-current/manual-review.json \
        --output eval/reports/correction-current/report.md
"""
import argparse
import json
from collections import defaultdict
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
    return {"supported": supported, "total": total,
            "support_rate": supported / total}


# Human-review pass conditions. A question only counts as passing the
# manual accuracy / completeness / citation gate when ALL of these hold:
#   - human_review.reviewed is true (and reviewer_kind is explicitly
#     recorded so an AI review cannot be confused with a human one);
#   - the run finished with run_status = success;
#   - refusal_supported is true (only meaningful for unanswerable
#     questions; answerable questions are graded on the other fields);
#   - fabricated_content is false;
#   - fabricated_citation is false.
# The review template's _null_rule still applies: a metric that cannot
# be computed is recorded as null and excluded from the denominator,
# never coerced to a 100% to make the headline number look healthy.
def _passes_manual_gate(hr: dict) -> bool:
    return bool(
        hr.get("reviewed")
        and hr.get("run_status", "success") == "success"
        and hr.get("refusal_supported", True)
        and not hr.get("fabricated_content", False)
        and not hr.get("fabricated_citation", False)
    )


def _strategy_bucket(runs: list) -> dict[str, list]:
    """Group runs by their `strategy` field.

    A run that does not carry the field is treated as the legacy
    "agent" run, so a file written by an older run_eval.py still parses
    and contributes to the agent bucket.
    """
    out: dict[str, list] = defaultdict(list)
    for r in runs:
        out[r.get("strategy", "agent")].append(r)
    return out


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

    by_strategy = _strategy_bucket(runs)
    run_status_counts: dict[str, int] = defaultdict(int)
    for r in runs:
        run_status_counts[r.get("run_status", "success")] += 1

    lines: list[str] = ["# WeKnora MCP 验证评估报告", ""]
    lines.append(f"总记录数：{len(runs)}（按策略：{dict((k, len(v)) for k, v in by_strategy.items())}）")
    lines.append(f"运行状态分布：{dict(run_status_counts)}")
    lines.append("")
    lines.append("> 仅 `run_status = success` 的记录进入下面的召回/证据分母；"
                 "timeout / model_error / config_error / tool_error 在运行状态分布里单独可见，"
                 "绝不并入召回分母抹平。")
    lines.append("")

    # Per-strategy metrics. Each block lists the macro average of the
    # answerable questions' document recall, all-found, evidence recall.
    # Unanswerable questions are not part of any recall denominator
    # (their required set is empty), but the section above still
    # reports their raw counts.
    for strategy, strat_runs in by_strategy.items():
        doc_recalls, all_found, evidence_recalls = [], [], []
        answerable = 0
        unanswerable = 0
        for run in strat_runs:
            if run.get("run_status") != "success":
                continue
            required_docs = run.get("required_documents", [])
            found_doc_ids = {d.replace("doc", "") for d in run.get("found_documents", [])}
            required_evidence = run.get("required_evidence") or []
            hit_evidences = run.get("hit_evidence") or []
            if not required_docs:
                unanswerable += 1
                continue
            answerable += 1
            dm = document_metrics({d.replace("doc", "") for d in required_docs}, found_doc_ids)
            em = evidence_metrics(required_evidence, hit_evidences)
            if dm["recall"] is not None:
                doc_recalls.append(dm["recall"])
            if dm["all_found"] is not None:
                all_found.append(dm["all_found"])
            if em["recall"] is not None:
                evidence_recalls.append(em["recall"])
        lines.append(f"## 策略 {strategy}")
        lines.append(f"- 可回答题数：{answerable}，无答案题数：{unanswerable}")
        lines.append(f"- 文档 recall（宏平均）：{macro_average(doc_recalls)}")
        lines.append(f"- 文档找齐率：{sum(all_found)}/{len(all_found)}")
        lines.append(f"- 必需证据 recall（宏平均，仅 exact-text 命中）：{macro_average(evidence_recalls)}")
        lines.append("")

    # Human-review metrics: only count questions the reviewer actually
    # marked reviewed=true with the manual gate satisfied. A question
    # with a partial review (some fields null) is still graded where the
    # metrics are computable; the _null_rule applies to the field, not
    # the whole question.
    manual = review.get("questions", {}) if isinstance(review, dict) else {}
    fact_acc, fact_comp, cit_sup = [], [], []
    reviewed = 0
    reviewer_kinds: dict[str, int] = defaultdict(int)
    manual_status: dict[str, int] = defaultdict(int)
    for qid, entry in manual.items():
        hr = entry.get("human_review") or {}
        if not hr.get("reviewed"):
            continue
        reviewed += 1
        rk = hr.get("reviewer_kind") or "unspecified"
        reviewer_kinds[rk] += 1
        if not _passes_manual_gate(hr):
            manual_status["failed_gate"] += 1
            continue
        manual_status["passed_gate"] += 1
        if hr.get("verifiable_claims"):
            fact_acc.append(hr.get("correct_claims", 0) / hr["verifiable_claims"])
        if hr.get("necessary_facts_total"):
            fact_comp.append(hr.get("covered_facts", 0) / hr["necessary_facts_total"])
        if hr.get("total_cited_claims"):
            cit_sup.append(hr.get("supported_cited_claims", 0) / hr["total_cited_claims"])

    lines.append("## 人工复核指标")
    lines.append(f"已复核题数：{reviewed}/{len(manual)}")
    lines.append(f"复核人类型分布：{dict(reviewer_kinds)}")
    lines.append(f"通过门控题数：{manual_status.get('passed_gate', 0)}，未通过门控：{manual_status.get('failed_gate', 0)}")
    lines.append(f"事实准确率（宏平均，门控通过后）：{macro_average(fact_acc)}")
    lines.append(f"事实完整率（宏平均，门控通过后）：{macro_average(fact_comp)}")
    lines.append(f"引用支持率（宏平均，门控通过后）：{macro_average(cit_sup)}")
    if reviewed == 0:
        lines.append("")
        lines.append("> 尚未有人工复核，事实准确率 / 完整率 / 引用支持率为 not_evaluated。")
    if reviewed > 0 and not manual_status.get("passed_gate"):
        lines.append("> 复核已记录，但没有任何题目通过门控；事实指标分母为零，按 not_evaluated 报告。")
    if reviewed > 0 and not reviewer_kinds.get("human"):
        lines.append("> 复核均由 AI 完成；不可声称为人工复核。")
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
