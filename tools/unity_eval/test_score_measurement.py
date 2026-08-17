import contextlib
import io
import json
import hashlib
import math
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))

import score_measurement
import measure_cases


class ScoreMeasurementTests(unittest.TestCase):
    def write_grading_input(self, root: Path, batches: list[dict]):
        rendered = "".join(
            score_measurement._canonical(batch).decode() + "\n" for batch in batches
        )
        (root / "grading-input.jsonl").write_text(rendered, encoding="utf-8")
        input_hash = hashlib.sha256((root / "grading-input.jsonl").read_bytes()).hexdigest()
        count = sum(len(batch["items"]) for batch in batches)
        (root / "grading-input.manifest.json").write_text(json.dumps({
            "schema_version": 1,
            "grading_input_sha256": input_hash,
            "results_sha256": "0" * 64,
            "gold_sha256": "0" * 64,
            "batch_count": len(batches),
            "run_count": count,
        }), encoding="utf-8")

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
            "worker_output": {"answerable": answerable, "answer": "a", "evidence": [],
                              "searches": [], "retrievals": retrievals, "notes": ""},
            "tool_counts": {"shell__shell_command": number},
            "ragrep_search_count": 0 if condition == "c0_no_skill_no_ragrep" else 1,
            "rg_count": number % 2,
            "file_read_count": number + 1,
            "duration_seconds": number,
            "tokens": {"input_tokens": number * 10, "cached_input_tokens": number,
                       "output_tokens": number * 2, "reasoning_output_tokens": number * 3,
                       "total_tokens": number * 12},
            "expected_correct": correct,
        }

    def gold(self, identifier, split, answerable):
        return {
            "id": identifier,
            "answer": "gold" if answerable else "",
            "required_points": ["p"] if answerable else [],
            "evidence": ([{"doc": "Manual/a.md", "para": 1}] if answerable else []),
            "type": "direct" if answerable else "unanswerable",
            "domain": "ui",
            "split": split,
            "answerable": answerable,
            "unanswerable_kind": None if answerable else "corpus_outside",
        }

    def grade(self, result):
        correct = result["expected_correct"]
        answerable = result["id"] == "q1"
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

    def test_validate_grade_rejects_windows_and_mixed_traversal(self):
        result = self.result("c0_no_skill_no_ragrep", "q1", "holdout", True, True, 1)
        for doc in (r"..\secret.md", r"\secret.md", r"C:secret.md", r"Manual/..\secret.md"):
            grade = self.grade(result)
            grade["gold_evidence"] = [{"doc": doc, "para": 0}]
            with self.subTest(doc=doc), self.assertRaisesRegex(ValueError, "evidence"):
                score_measurement.validate_grade(
                    grade, result["run_key"], expected_points=["p"], answerable=True,
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
        grading_items = {
            result["run_key"]: {
                "run_key": result["run_key"],
                "result": {key: result.get(key) for key in ("run_key", "id", "status", "worker_output")},
                "gold": self.gold(result["id"], result["split"], result["id"] == "q1"),
            }
            for result in results
        }
        report = score_measurement.aggregate(results, grades, grading_items, seed=7)
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
        self.assertEqual(set(report["by_repetition"]), {"1"})
        self.assertIn("wrong_evidence_adoption_rate", report["retrieval"])
        self.assertEqual(set(report["per_question"]), {"q1", "q2"})
        self.assertIn("duration_seconds", report["by_condition"]["c1_ragrep_no_skill"])
        self.assertIn("tokens", report["by_condition"]["c1_ragrep_no_skill"])

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
            self.assertTrue((output / "grading-input.manifest.json").is_file())
            with self.assertRaisesRegex(ValueError, "exists"):
                score_measurement.prepare_grading(results, gold, output)
            grade = {
                "run_key": "c0/r1/q1", "correct": True,
                "required_points": [{"point": "p", "met": True}],
                "contradiction": False, "unsupported_claim": False,
                "unanswerable_correct": None, "failure_class": None,
                "reason": "ok", "gold_evidence": [],
            }
            score_measurement.assign_grader(
                output, "grades-a", 1, "grader-thread-a",
                datetime(2026, 8, 12, tzinfo=timezone.utc),
            )
            score_measurement.assign_grader(
                output, "grades-b", 1, "grader-thread-b",
                datetime(2026, 8, 12, tzinfo=timezone.utc),
            )
            score_measurement.record_grade(output, "grades-a", grade, thread_id="grader-thread-a")
            score_measurement.record_grade(output, "grades-b", grade, thread_id="grader-thread-b")
            self.assertEqual(len((output / "grades-a.sha256-chain.jsonl").read_text().splitlines()), 1)
            with self.assertRaisesRegex(ValueError, "duplicate"):
                score_measurement.record_grade(output, "grades-a", grade, thread_id="grader-thread-a")
            self.assertTrue(score_measurement.verify_grade_ledger(output, "grades-a")["valid"])
            event = score_measurement.mark_grader_archived(
                output, "grades-a", "grader-thread-a",
                datetime(2026, 8, 12, tzinfo=timezone.utc),
            )
            self.assertEqual(event["thread_id"], "grader-thread-a")
            with self.assertRaisesRegex(ValueError, "duplicate"):
                score_measurement.mark_grader_archived(
                    output, "grades-a", "grader-thread-a",
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
            self.write_grading_input(root, [item])
            a = {"run_key": run_key, "correct": False, "required_points": [],
                 "contradiction": False, "unsupported_claim": False,
                 "unanswerable_correct": False, "failure_class": "unanswerable_failure",
                 "reason": "wrong", "gold_evidence": []}
            b = dict(a, correct=True, unanswerable_correct=True, failure_class=None)
            assignments = [
                {"ledger": "grades-a", "batch": 1, "run_keys": [run_key], "thread_id": "grader-a", "assigned_at": "2026-08-12T00:00:00Z"},
                {"ledger": "grades-b", "batch": 1, "run_keys": [run_key], "thread_id": "grader-b", "assigned_at": "2026-08-12T00:00:00Z"},
            ]
            (root / "grader-assignments.jsonl").write_text(
                "".join(json.dumps(row) + "\n" for row in assignments), encoding="utf-8",
            )
            score_measurement.record_grade(root, "grades-a", a, thread_id="grader-a")
            with self.assertRaisesRegex(ValueError, "missing"):
                score_measurement.verify_grades(root)
            score_measurement.record_grade(root, "grades-b", b, thread_id="grader-b")
            with self.assertRaisesRegex(ValueError, "missing adjudication"):
                score_measurement.verify_grades(root)
            unrelated = dict(a, run_key="c0/r1/other")
            with self.assertRaisesRegex(ValueError, "adjudication"):
                score_measurement.record_grade(
                    root, "adjudication", unrelated, thread_id="adjudicator"
                )
            score_measurement.prepare_adjudication(root)
            score_measurement.assign_grader(
                root, "adjudication", 1, "adjudicator", datetime.now(timezone.utc)
            )
            score_measurement.record_grade(
                root, "adjudication", a, thread_id="adjudicator"
            )

    def test_verify_grades_replays_adjudication_chain_and_rejects_extra_rows(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            run_key = "c0/r1/q1"
            item = {"batch": 1, "items": [{
                "run_key": run_key,
                "result": {"run_key": run_key, "id": "q1", "status": "completed", "worker_output": {}},
                "gold": {key: value for key, value in self.gold("q1", "holdout", True).items() if key != "id"},
            }]}
            self.write_grading_input(root, [item])
            grade = {"run_key": run_key, "correct": True,
                     "required_points": [{"point": "p", "met": True}],
                     "contradiction": False, "unsupported_claim": False,
                     "unanswerable_correct": None, "failure_class": None,
                     "reason": "ok", "gold_evidence": []}
            for ledger, thread in (("grades-a", "grader-a"), ("grades-b", "grader-b")):
                score_measurement.assign_grader(
                    root, ledger, 1, thread, datetime.now(timezone.utc)
                )
            score_measurement.record_grade(root, "grades-a", grade, thread_id="grader-a")
            score_measurement.record_grade(root, "grades-b", dict(grade, correct=False,
                required_points=[{"point": "p", "met": False}], failure_class="reading_failure"),
                thread_id="grader-b")
            score_measurement.prepare_adjudication(root)
            score_measurement.assign_grader(
                root, "adjudication", 1, "adjudicator", datetime.now(timezone.utc)
            )
            score_measurement.record_grade(
                root, "adjudication", grade, thread_id="adjudicator"
            )
            (root / "adjudication.sha256-chain.jsonl").write_text(
                json.dumps({"corrupt": True}) + "\n", encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "chain"):
                score_measurement.verify_grades(root)
            first_chain = score_measurement._chain_row(grade, score_measurement.GENESIS_SHA256)
            extra = dict(grade, run_key="c0/r1/extra")
            extra_chain = score_measurement._chain_row(extra, first_chain["chain_sha256"])
            (root / "adjudication.jsonl").write_text(
                json.dumps(grade) + "\n" + json.dumps(extra) + "\n", encoding="utf-8",
            )
            (root / "adjudication.sha256-chain.jsonl").write_text(
                json.dumps(first_chain) + "\n" + json.dumps(extra_chain) + "\n", encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "extra adjudication"):
                score_measurement.verify_grades(root)
            duplicate_chain = score_measurement._chain_row(grade, first_chain["chain_sha256"])
            (root / "adjudication.jsonl").write_text(
                json.dumps(grade) + "\n" + json.dumps(grade) + "\n", encoding="utf-8",
            )
            (root / "adjudication.sha256-chain.jsonl").write_text(
                json.dumps(first_chain) + "\n" + json.dumps(duplicate_chain) + "\n", encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "duplicate"):
                score_measurement.verify_grades(root)

    def test_verify_grades_requires_all_assignments_to_be_archived(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            results = [{"run_key": "c0/r1/q1", "id": "q1", "status": "completed", "worker_output": {}}]
            score_measurement.prepare_grading(results, [self.gold("q1", "holdout", True)], root / "private")
            run_dir = root / "private"
            grade = {"run_key": "c0/r1/q1", "correct": True,
                     "required_points": [{"point": "p", "met": True}],
                     "contradiction": False, "unsupported_claim": False,
                     "unanswerable_correct": None, "failure_class": None,
                     "reason": "ok", "gold_evidence": []}
            for ledger, thread in (("grades-a", "grader-a"), ("grades-b", "grader-b")):
                score_measurement.assign_grader(
                    run_dir, ledger, 1, thread,
                    datetime(2026, 8, 12, tzinfo=timezone.utc),
                )
                score_measurement.record_grade(run_dir, ledger, grade, thread_id=thread)
            with self.assertRaisesRegex(ValueError, "unarchived"):
                score_measurement.verify_grades(run_dir)

    def test_real_result_to_resolved_grade_to_report_uses_immutable_gold(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            run_dir = root / "run"
            run_dir.mkdir()
            run_key = "c1_ragrep_no_skill/r1/q1"
            dispatch = {"run_key": run_key, "condition": "c1_ragrep_no_skill",
                        "repetition": 1, "order": 1, "id": "q1", "query": "q", "split": "holdout"}
            (run_dir / "dispatch.jsonl").write_text(json.dumps(dispatch) + "\n", encoding="utf-8")
            sources = {}
            for name in ("frozen_manifest", "holdout_questions", "development", "holdout_gold", "validator", "skill", "db", "binary", "wrapper"):
                path = root / name; path.write_text(name, encoding="utf-8"); sources[name] = str(path.resolve())
            prompts = {}
            for condition in measure_cases.CONDITIONS:
                path = root / f"{condition}.prompt"; path.write_text(condition, encoding="utf-8"); prompts[condition] = str(path.resolve())
            controller_inputs = root / "controller-inputs.json"
            controller_inputs.write_text(json.dumps({"sources": sources, "prompts": prompts}), encoding="utf-8")
            (run_dir / "manifest.json").write_text(json.dumps({
                "model": "gpt-5.6-sol", "reasoning": "medium", "timeout_seconds": 600,
                "prompt_hashes": {key: measure_cases.sha256_file(Path(value)) for key, value in prompts.items()},
                "sha256": {key: measure_cases.sha256_file(Path(value)) for key, value in sources.items()},
                "skill_path": sources["skill"], "wrapper_path": sources["wrapper"],
                "controller_inputs_sha256": measure_cases.sha256_file(controller_inputs),
            }), encoding="utf-8")
            transcript = root / "transcript.jsonl"
            from test_measure_cases import TranscriptTests
            TranscriptTests().write_transcript(transcript, thread_id="worker-1", calls=[
                ("shell", "shell_command", {"command": f"& '{sources['wrapper']}' search q"}, "ok"),
            ])
            final = json.dumps({
                "id": "q1", "answerable": False, "answer": "確認できない", "evidence": [],
                "searches": ["q"], "retrievals": [{"query": "q", "mode": "hybrid", "hits": [
                    {"rank": 1, "doc": "Manual/a.md", "para": 1}
                ]}], "notes": "",
            }, ensure_ascii=False)
            measure_cases.claim_runs(
                run_dir, controller_inputs, 1,
                datetime(2026, 8, 12, 0, tzinfo=timezone.utc),
            )
            measure_cases.assign_claim(
                run_dir, run_key, "worker-1",
                datetime(2026, 8, 12, 0, 30, tzinfo=timezone.utc),
            )
            record = measure_cases.record_result(
                run_dir, run_key, "worker-1", "completed", final, transcript,
                datetime(2026, 8, 12, 1, tzinfo=timezone.utc), controller_inputs=controller_inputs,
            )
            grading_dir = root / "grading"
            score_measurement.prepare_grading([record], [self.gold("q1", "holdout", True)], grading_dir)
            a = {"run_key": run_key, "correct": False,
                 "required_points": [{"point": "p", "met": False}],
                 "contradiction": False, "unsupported_claim": False,
                 "unanswerable_correct": None, "failure_class": "reading_failure",
                 "reason": "a", "gold_evidence": [{"doc": "Manual/wrong.md", "para": 9}]}
            b = dict(a, correct=True, required_points=[{"point": "p", "met": True}],
                     failure_class=None, reason="b")
            for ledger, thread, grade in (("grades-a", "grader-a", a), ("grades-b", "grader-b", b)):
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(score_measurement.main([
                        "assign-grader", "--run-dir", str(grading_dir), "--ledger", ledger,
                        "--batch", "1", "--thread-id", thread,
                        "--assigned-at", "2026-08-12T02:00:00Z",
                    ]), 0)
                with mock.patch("sys.stdin", io.StringIO(json.dumps(grade))), contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(score_measurement.main([
                        "record-grade", "--run-dir", str(grading_dir), "--ledger", ledger,
                        "--thread-id", thread,
                    ]), 0)
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(score_measurement.main([
                        "mark-grader-archived", "--run-dir", str(grading_dir), "--ledger", ledger,
                        "--thread-id", thread, "--archived-at", "2026-08-12T03:00:00Z",
                    ]), 0)
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(score_measurement.main([
                    "prepare-adjudication", "--run-dir", str(grading_dir), "--batch-size", "25",
                ]), 0)
                self.assertEqual(score_measurement.main([
                    "assign-grader", "--run-dir", str(grading_dir), "--ledger", "adjudication",
                    "--batch", "1", "--thread-id", "adjudicator",
                    "--assigned-at", "2026-08-12T04:00:00Z",
                ]), 0)
            with mock.patch("sys.stdin", io.StringIO(json.dumps(a))), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(score_measurement.main([
                    "record-grade", "--run-dir", str(grading_dir), "--ledger", "adjudication",
                    "--thread-id", "adjudicator",
                ]), 0)
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(score_measurement.main([
                    "mark-grader-archived", "--run-dir", str(grading_dir), "--ledger", "adjudication",
                    "--thread-id", "adjudicator", "--archived-at", "2026-08-12T05:00:00Z",
                ]), 0)
            verify_stdout = io.StringIO()
            with contextlib.redirect_stdout(verify_stdout):
                self.assertEqual(score_measurement.main([
                    "verify-grades", "--run-dir", str(grading_dir),
                ]), 0)
            summary = json.loads(verify_stdout.getvalue())
            self.assertEqual(summary["resolved_count"], 1)
            output = root / "report"
            results_path = root / "results.jsonl"
            results_path.write_text(json.dumps(record) + "\n", encoding="utf-8")
            report_stdout = io.StringIO()
            with contextlib.redirect_stdout(report_stdout):
                self.assertEqual(score_measurement.main([
                    "report", "--run-dir", str(grading_dir), "--results", str(results_path),
                    "--seed", "7", "--output", str(output),
                ]), 0)
            report = json.loads(report_stdout.getvalue())
            self.assertEqual(report["overall_accuracy"], 0.0)
            self.assertEqual(report["retrieval"]["recall_at_5"], 1.0)
            self.assertEqual(report["unanswerable_rejection_rate"], None)
            self.assertTrue((output / "summary.json").is_file())
            self.assertTrue((output / "by-condition.csv").is_file())
            self.assertTrue((output / "by-repetition.csv").is_file())
            self.assertTrue((output / "per-question.csv").is_file())
            tampered = dict(record, duration_seconds=999999)
            with self.assertRaisesRegex(ValueError, "results hash"):
                score_measurement.report(
                    grading_dir, [tampered], seed=7, output=root / "tampered-report"
                )


if __name__ == "__main__":
    unittest.main()
