#!/usr/bin/env python3
"""Pre-fill the manual-review file from runs.jsonl so the reviewer only
fills in the four numeric fields (事实准确率/完整率/引用支持率/无答案)
and the per-claim judgement.

This is convenience: it copies the template, drops in the per-question
final_answer and the agent's tool calls, and leaves the human fields
blank. A 0/0 entry is also stamped so the reviewer can mark "not
reviewed yet" explicitly.
"""
import argparse
import json
import shutil
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=Path,
                        default=_ROOT / "eval/reports/runs.jsonl")
    parser.add_argument("--template", type=Path,
                        default=_ROOT / "eval/reports/manual-review.template.json")
    parser.add_argument("--out", type=Path,
                        default=_ROOT / "eval/reports/manual-review.json")
    parser.add_argument("--questions", type=Path,
                        default=_ROOT / "eval/questions/demo-12.json")
    args = parser.parse_args()

    template = json.loads(args.template.read_text(encoding="utf-8"))
    questions = {q["id"]: q for q in json.loads(args.questions.read_text(encoding="utf-8"))["questions"]}
    runs = []
    with args.runs.open(encoding="utf-8") as src:
        for line in src:
            if line.strip():
                runs.append(json.loads(line))

    filled: dict = {
        "_purpose": template.get("_purpose", []),
        "_how_to_use": template.get("_how_to_use", []),
        "_formulas": template.get("_formulas", {}),
        "_null_rule": template.get("_null_rule", "未复核时 metrics 字段为 null；不要写 0。"),
        "questions": {},
    }
    for r in runs:
        qid = r["question_id"]
        q = questions.get(qid, {})
        filled["questions"][qid] = {
            "category": r.get("category"),
            "answerable": r.get("answerable", True),
            "question": r.get("question"),
            "required_documents": r.get("required_documents", []),
            "required_evidence": r.get("required_evidence", []),
            "expected_facts": q.get("expected_facts", []),
            "tool_call_names": r.get("tool_call_names", []),
            "cited_documents": r.get("found_documents", []),
            "cited_chunks": r.get("cited_chunks", []),
            "final_answer": r.get("final_answer"),
            "human_review": {
                "reviewed": False,
                "verifiable_claims": 0,
                "correct_claims": 0,
                "necessary_facts_total": len(q.get("expected_facts", [])),
                "covered_facts": 0,
                "supported_cited_claims": 0,
                "total_cited_claims": 0,
                "metric_fact_accuracy": None,
                "metric_fact_completeness": None,
                "metric_citation_support": None,
                "metric_no_answer_handled": None,
                "notes": "",
            },
        }
    args.out.write_text(json.dumps(filled, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"manual-review seeded at {args.out} ({len(filled['questions'])} questions)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
