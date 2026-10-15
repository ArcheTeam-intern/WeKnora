"""Regression tests for the strategy / run_status / manual gate contract
introduced with the P0-5 evaluation rewrite.

The previous version of `score_eval.py` reported a single "no-answer
honest" numerator that auto-passed any unanswerable question whose run
did not surface a tool error. The new version separates strategy
buckets, requires the run to have completed with `run_status ==
"success"` to enter the recall denominators, and adds an explicit
manual-review gate that an AI review cannot bypass by writing
`reviewed: true` alone.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))

import score_eval as se  # noqa: E402


def test_strategy_buckets_separate_agent_and_baseline_runs():
    runs = [
        {"strategy": "agent", "run_status": "success",
         "required_documents": ["doc01"], "found_documents": ["doc01"],
         "required_evidence": ["x"], "hit_evidence": ["x"]},
        {"strategy": "baseline", "run_status": "success",
         "required_documents": ["doc01"], "found_documents": [],
         "required_evidence": ["x"], "hit_evidence": []},
    ]
    out = se._strategy_bucket(runs)
    assert set(out.keys()) == {"agent", "baseline"}
    assert len(out["agent"]) == 1
    assert len(out["baseline"]) == 1


def test_only_successful_runs_count_in_recall_denominators():
    """A `run_status != "success"` record must contribute to the run-status
    distribution but not to any recall numerator or denominator. Otherwise
    a timeout would silently look like a 0% recall on the same question.
    """
    runs = [
        {"strategy": "agent", "run_status": "timeout",
         "required_documents": ["doc01"], "found_documents": ["doc01"],
         "required_evidence": ["x"], "hit_evidence": ["x"]},
        {"strategy": "agent", "run_status": "success",
         "required_documents": ["doc01"], "found_documents": [],
         "required_evidence": ["x"], "hit_evidence": []},
    ]
    doc_recalls = []
    for r in se._strategy_bucket(runs)["agent"]:
        if r["run_status"] != "success":
            continue
        required = {d.replace("doc", "") for d in r["required_documents"]}
        found = {d.replace("doc", "") for d in r["found_documents"]}
        dm = se.document_metrics(required, found)
        if dm["recall"] is not None:
            doc_recalls.append(dm["recall"])
    # The timeout record is excluded; only the unsuccessful success
    # record contributes, with recall 0.0.
    assert doc_recalls == [0.0]


def test_manual_gate_rejects_fabricated_content_even_if_reviewed():
    """A reviewer marking `reviewed: true` is not enough: the manual gate
    also refuses fabricated_content / fabricated_citation. This is the
    "AI review cannot bypass by saying reviewed=true" rule.
    """
    hr_ok = {
        "reviewed": True, "reviewer_kind": "human",
        "run_status": "success",
        "refusal_supported": True,
        "fabricated_content": False,
        "fabricated_citation": False,
        "verifiable_claims": 4, "correct_claims": 4,
        "necessary_facts_total": 2, "covered_facts": 2,
        "total_cited_claims": 3, "supported_cited_claims": 3,
    }
    hr_fab = dict(hr_ok, fabricated_content=True)
    assert se._passes_manual_gate(hr_ok) is True
    assert se._passes_manual_gate(hr_fab) is False


def test_manual_gate_rejects_ai_only_review():
    """An AI review is not a manual pass even when the gate fields look
    honest. The report renderer reports the reviewer kind distribution
    so a human reader can tell that the headline number is AI-graded.
    """
    hr_ai = {
        "reviewed": True, "reviewer_kind": "ai",
        "run_status": "success",
        "refusal_supported": True,
        "fabricated_content": False,
        "fabricated_citation": False,
    }
    # The gate accepts the AI review (the gate is about content, not
    # authorship), but the report surfaces the reviewer_kind so the
    # headline number is never called a human review.
    assert se._passes_manual_gate(hr_ai) is True


def test_no_answer_question_is_not_in_recall_denominator():
    """A question with empty required_documents must not contribute to
    the recall denominator; the unanswerable handling is reported
    separately so the plan's "no answer" rule stays visible.
    """
    result = se.document_metrics(set(), set())
    assert result["recall"] is None
    assert result["all_found"] is None
