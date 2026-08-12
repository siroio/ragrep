import json
import math
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

import score_measurement


class ScoreMeasurementTests(unittest.TestCase):
    def result(self, condition, identifier, split, answerable, correct, number):
        gold_doc = "Manual/a.md" if identifier == "q1" else None
        retrievals = []
        if condition != "c0_no_skill_no_ragrep":
            hit_doc = gold_doc if condition == "c1_ragrep_no_skill" and identifier == "q1" else "Manual/x.md"
            retrievals = [{"query": "q", "mode": "hybrid", "hits": [
                {"rank": 1, "doc": hit_doc, "para": 1}
            ]}]
        return {
            "run_key": f"{condition}/r1/{identifier}", "condition": condition,
            "repetition": 1, "id": identifier, "split": split,
            "type": "direct" if answerable else "unanswerable", "domain": "ui",
            "answerable": answerable, "unanswerable_kind": None if answerable else "corpus_outside",
            "worker_output": {"answerable": answerable, "answer": "a", "evidence": [],
                              "searches": [], "retrievals": retrievals, "notes": ""},
            "duration_seconds": number,
            "tokens": {"input_tokens": number * 10, "cached_input_tokens": number,
                       "output_tokens": number * 2, "reasoning_output_tokens": number * 3,
                       "total_tokens": number * 12},
            "expected_correct": correct,
        }

    def grade(self, result):
        correct = result["expected_correct"]
        answerable = result["answerable"]
        return {
            "run_key": result["run_key"], "correct": correct,
            "required_points": ([{"point": "p", "met": correct}] if answerable else []),
            "contradiction": False, "unsupported_claim": False,
            "unanswerable_correct": (None if answerable else correct),
            "failure_class": (None if correct else ("selection_failure" if answerable else "unanswerable_failure")),
            "reason": "短い理由",
            "gold_evidence": ([{"doc": "Manual/a.md", "para": 1}] if answerable else []),
        }

    def test_make_grading_batches_isolates_and_stably_orders(self):
        results = [
            {"run_key": "c1/r1/q2", "id": "q2", "status": "completed", "worker_output": {"answer": "b"}, "tool_calls": ["secret"]},
            {"run_key": "c0/r1/q1", "id": "q1", "status": "completed", "worker_output": {"answer": "a"}, "tool_calls": ["secret"]},
        ]
        gold = [
            {"id": "q1", "answer": "ga", "required_points": ["p1"], "evidence": [], "type": "direct",
             "domain": "ui", "split": "development", "answerable": True, "unanswerable_kind": None,
             "author": "private history"},
            {"id": "q2", "answer": "", "required_points": [], "evidence": [], "type": "unanswerable",
             "domain": "ui", "split": "holdout", "answerable": False, "unanswerable_kind": "corpus_outside"},
        ]
        batches = score_measurement.make_grading_batches(results, gold, batch_size=1)
        self.assertEqual([batch["items"][0]["run_key"] for batch in batches], ["c0/r1/q1", "c1/r1/q2"])
        self.assertEqual(set(batches[0]["items"][0]), {"run_key", "result", "gold"})
        self.assertEqual(set(batches[0]["items"][0]["result"]), {"run_key", "id", "status", "worker_output"})
        self.assertNotIn("author", json.dumps(batches, ensure_ascii=False))
        self.assertNotIn("secret", json.dumps(batches, ensure_ascii=False))

    def test_validate_grade_enforces_exact_contextual_schema(self):
        result = self.result("c0_no_skill_no_ragrep", "q1", "holdout", True, True, 1)
        valid = self.grade(result)
        self.assertEqual(score_measurement.validate_grade(
            valid, result["run_key"], expected_points=["p"], answerable=True
        ), valid)
        variants = []
        extra = dict(valid, extra=True); variants.append(extra)
        unmet = json.loads(json.dumps(valid)); unmet["required_points"][0]["met"] = False; variants.append(unmet)
        missing = json.loads(json.dumps(valid)); missing["required_points"] = []; variants.append(missing)
        failure = dict(valid, correct=False, failure_class="magic"); variants.append(failure)
        long_reason = dict(valid, reason="x" * 501); variants.append(long_reason)
        wrong_shape = dict(valid, unanswerable_correct=True); variants.append(wrong_shape)
        for value in variants:
            with self.subTest(value=value):
                with self.assertRaises(ValueError):
                    score_measurement.validate_grade(
                        value, result["run_key"], expected_points=["p"], answerable=True
                    )

    def test_compare_grades_identifies_disagreement_and_audit(self):
        base = self.result("c0_no_skill_no_ragrep", "q1", "holdout", True, True, 1)
        agreed = self.grade(base)
        wrong = dict(agreed, correct=False, failure_class="reading_failure")
        unanswerable = self.result("c0_no_skill_no_ragrep", "q2", "holdout", False, True, 2)
        unanswerable_grade = self.grade(unanswerable)
        comparison = score_measurement.compare_grades(
            [agreed, unanswerable_grade], [wrong, unanswerable_grade]
        )
        self.assertEqual(comparison["needs_adjudication"], [base["run_key"]])
        self.assertEqual(comparison["agreed"], [unanswerable["run_key"]])
        self.assertEqual(comparison["audit_run_keys"], [base["run_key"], unanswerable["run_key"]])

    def test_aggregate_computes_grouped_retrieval_and_token_metrics(self):
        conditions = ("c0_no_skill_no_ragrep", "c1_ragrep_no_skill", "c2_skill_ragrep")
        correctness = ((True, True, False), (True, False, True))
        results = []
        number = 0
        for q_index, (identifier, split, answerable) in enumerate((("q1", "holdout", True), ("q2", "development", False))):
            for c_index, condition in enumerate(conditions):
                number += 1
                results.append(self.result(condition, identifier, split, answerable, correctness[q_index][c_index], number))
        grades = [self.grade(result) for result in results]
        report = score_measurement.aggregate(results, grades, seed=7)
        self.assertEqual(report["overall_accuracy"], 4 / 6)
        self.assertEqual(report["holdout_accuracy"], 2 / 3)
        self.assertEqual(report["unanswerable_rejection_rate"], 2 / 3)
        self.assertEqual(report["condition_delta_vs_c0"]["c1_ragrep_no_skill"], -0.5)
        self.assertEqual(report["condition_delta_vs_c0"]["c2_skill_ragrep"], -0.5)
        self.assertEqual(report["c0_correct_retrieval_incorrect_count"], 2)
        self.assertEqual(report["retrieval"]["recall_at_5"], 0.5)
        self.assertEqual(report["retrieval"]["recall_at_10"], 0.5)
        self.assertEqual(report["retrieval"]["evidence_acquired_rate"], 0.5)
        self.assertEqual(report["retrieval"]["accuracy_given_evidence"], 1.0)
        self.assertEqual(report["retrieval_by_condition"]["c1_ragrep_no_skill"]["recall_at_5"], 1.0)
        self.assertEqual(report["retrieval_by_condition"]["c2_skill_ragrep"]["recall_at_5"], 0.0)
        self.assertEqual(report["duration_seconds"]["mean"], 3.5)
        self.assertEqual(report["duration_seconds"]["median"], 3.5)
        self.assertTrue(math.isclose(report["duration_seconds"]["pstdev"], math.sqrt(35 / 12)))
        self.assertEqual(report["accuracy_bootstrap_95"], [2 / 3, 2 / 3])
        self.assertEqual(report["tokens"]["input_tokens"]["mean"], 35)
        self.assertEqual(report["tokens"]["cached_input_tokens"]["mean"], 3.5)
        self.assertEqual(set(report["by_split"]), {"development", "holdout"})
        self.assertEqual(set(report["by_type"]), {"direct", "unanswerable"})
        self.assertEqual(report["failure_class_counts"], {"selection_failure": 1, "unanswerable_failure": 1})

    def test_prepare_grading_and_grade_ledgers_are_append_only(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            results = [{"run_key": "c0/r1/q1", "id": "q1", "status": "completed",
                        "worker_output": {"answer": "a"}}]
            gold = [{"id": "q1", "answer": "ga", "required_points": ["p"], "evidence": [],
                     "type": "direct", "domain": "ui", "split": "holdout",
                     "answerable": True, "unanswerable_kind": None}]
            output = root / "private-run"
            score_measurement.prepare_grading(results, gold, output)
            self.assertTrue((output / "grading-input.jsonl").is_file())
            with self.assertRaisesRegex(ValueError, "exists"):
                score_measurement.prepare_grading(results, gold, output)
            grade = {
                "run_key": "c0/r1/q1", "correct": True,
                "required_points": [{"point": "p", "met": True}],
                "contradiction": False, "unsupported_claim": False,
                "unanswerable_correct": None, "failure_class": None,
                "reason": "ok", "gold_evidence": [],
            }
            score_measurement.record_grade(output, "grades-a", grade)
            score_measurement.record_grade(output, "grades-b", grade)
            self.assertEqual(len((output / "grades-a.sha256-chain.jsonl").read_text().splitlines()), 1)
            with self.assertRaisesRegex(ValueError, "duplicate"):
                score_measurement.record_grade(output, "grades-a", grade)
            self.assertTrue(score_measurement.verify_grade_ledger(output, "grades-a")["valid"])
            event = score_measurement.mark_grader_archived(
                output, "grades-a", grade["run_key"], "grader-thread-a",
                datetime(2026, 8, 12, tzinfo=timezone.utc),
            )
            self.assertEqual(event["thread_id"], "grader-thread-a")
            with self.assertRaisesRegex(ValueError, "duplicate"):
                score_measurement.mark_grader_archived(
                    output, "grades-a", grade["run_key"], "grader-thread-a",
                    datetime.now(timezone.utc),
                )

    def test_verify_grades_requires_complete_pairs_and_limits_adjudication(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            run_key = "c0/r1/q1"
            item = {"batch": 1, "items": [{
                "run_key": run_key,
                "result": {"run_key": run_key, "id": "q1", "status": "completed", "worker_output": {}},
                "gold": {"answer": "", "required_points": [], "evidence": [], "type": "unanswerable",
                         "domain": "ui", "split": "holdout", "answerable": False,
                         "unanswerable_kind": "corpus_outside"},
            }]}
            (root / "grading-input.jsonl").write_text(json.dumps(item) + "\n")
            a = {"run_key": run_key, "correct": False, "required_points": [],
                 "contradiction": False, "unsupported_claim": False,
                 "unanswerable_correct": False, "failure_class": "unanswerable_failure",
                 "reason": "wrong", "gold_evidence": []}
            b = dict(a, correct=True, unanswerable_correct=True, failure_class=None)
            score_measurement.record_grade(root, "grades-a", a)
            with self.assertRaisesRegex(ValueError, "missing"):
                score_measurement.verify_grades(root)
            score_measurement.record_grade(root, "grades-b", b)
            summary = score_measurement.verify_grades(root)
            self.assertEqual(summary["needs_adjudication"], [run_key])
            self.assertEqual(summary["status"], "provisional")
            self.assertIn(run_key, (root / "audit.csv").read_text())
            unrelated = dict(a, run_key="c0/r1/other")
            with self.assertRaisesRegex(ValueError, "adjudication"):
                score_measurement.record_grade(root, "adjudication", unrelated)
            score_measurement.record_grade(root, "adjudication", a)


if __name__ == "__main__":
    unittest.main()
