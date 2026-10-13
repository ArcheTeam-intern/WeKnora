"""Unit tests for score_eval.py's metric functions (plan §8 examples)."""
import sys
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))

import score_eval as se  # noqa: E402


def test_document_recall_and_all_found_are_distinct():
    result = se.document_metrics({"d1", "d2"}, {"d1", "d3"})
    assert result == {"matched": 1, "required": 2, "recall": 0.5, "all_found": False}


def test_no_answer_question_is_not_perfect_recall():
    result = se.document_metrics(set(), set())
    assert result["recall"] is None
    assert result["all_found"] is None


def test_all_found_true_when_every_required_doc_found():
    result = se.document_metrics({"d1", "d2"}, {"d1", "d2", "d3"})
    assert result["all_found"] is True
    assert result["recall"] == 1.0


def test_evidence_metrics_only_counts_read_evidence():
    result = se.evidence_metrics({"e1", "e2"}, {"e1"})
    assert result == {"matched": 1, "required": 2, "recall": 0.5}


def test_macro_average_ignores_unanswerable():
    assert se.macro_average([0.5, None, 1.0]) == 0.75


def test_fact_metrics_empty_denominator_is_none():
    assert se.fact_metrics(0, 0)["accuracy"] is None


def test_citation_support_unknown_does_not_count_as_supported():
    result = se.citation_support_metrics(8, 10)
    assert result == {"supported": 8, "total": 10, "support_rate": 0.8}
