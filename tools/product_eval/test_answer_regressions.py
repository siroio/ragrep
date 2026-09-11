import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import answer_regressions


FIXTURES = Path(__file__).with_name("answer_regressions_fixtures.jsonl")


class AnswerRegressionTests(unittest.TestCase):
    def test_blind_payload_omits_expected_labels_and_provenance(self):
        fixtures = answer_regressions.load_fixtures(FIXTURES)

        payload = answer_regressions.blind_payload(fixtures)
        encoded = json.dumps(payload, ensure_ascii=False)

        self.assertEqual(len(payload["cases"]), len(fixtures))
        self.assertEqual([item["id"] for item in payload["cases"]], [f"c0{index}" for index in range(1, 9)])
        self.assertTrue(all(item["answer"]["id"] == item["id"] for item in payload["cases"]))
        self.assertNotIn("expected_outcome", encoded)
        self.assertNotIn("expected_violations", encoded)
        self.assertNotIn("source_ref", encoded)
        self.assertNotIn("regression", encoded)
        self.assertNotIn("control", encoded)

    def test_load_fixtures_rejects_duplicate_ids(self):
        fixture = answer_regressions.load_fixtures(FIXTURES)[0]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "duplicate.jsonl"
            path.write_text("\n".join(json.dumps(fixture, ensure_ascii=False) for _ in range(2)) + "\n", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "duplicate id"):
                answer_regressions.load_fixtures(path)

    def test_grade_validation_requires_structured_boolean_fields(self):
        fixtures = answer_regressions.load_fixtures(FIXTURES)
        invalid = {"id": fixtures[0]["id"], "reason": "missing rubric fields"}

        with self.assertRaisesRegex(ValueError, "required_points_complete"):
            answer_regressions.evaluate_grades(fixtures, [invalid])

    def test_gate_uses_explicit_judge_grades_for_synthetic_pair(self):
        fixtures = [
            self._synthetic_fixture("c01", "regression", ["source_action_qualifier_fidelity"]),
            self._synthetic_fixture("c02", "pass", []),
        ]
        grades = [
            self._grade("c01", source_action_qualifier_fidelity=False),
            self._grade("c02"),
        ]

        result = answer_regressions.evaluate_grades(fixtures, grades)

        self.assertTrue(result["gate_pass"])
        self.assertEqual(result["summary"], {"cases": 2, "passed": 1, "regressions_detected": 1})

    def test_gate_requires_the_expected_violation_not_just_any_failure(self):
        fixture = self._synthetic_fixture("c01", "regression", ["source_action_qualifier_fidelity"])
        grade = self._grade("c01", required_points_complete=False)

        result = answer_regressions.evaluate_grades([fixture], [grade]) 

        self.assertFalse(result["gate_pass"])

    def test_fixture_outcome_requires_known_nonempty_or_empty_violations(self):
        fixture = self._synthetic_fixture("c01", "pass", ["source_action_qualifier_fidelity"])

        with self.assertRaisesRegex(ValueError, "expected_violations"):
            answer_regressions.load_fixtures_from_records([fixture])

    def test_output_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "result.json"
            output.write_text("keep", encoding="utf-8")
            with self.assertRaises(FileExistsError):
                answer_regressions._write_json(output, {"new": True})
            self.assertEqual(output.read_text(encoding="utf-8"), "keep")

    @staticmethod
    def _synthetic_fixture(fixture_id, outcome, violations):
        return {
            "id": fixture_id,
            "query": "q",
            "language": "en",
            "answerable": True,
            "passages": [{"doc": "a.md", "para": 0, "body": "source"}],
            "rubric": {
                "required_points": ["point"],
                "source_action_qualifier_fidelity": "preserve source",
                "language_matches_query": "answer in English",
                "correct_abstention": "do not abstain",
                "citations_valid": "cite source",
            },
            "answer": {"id": fixture_id, "answer": "answer", "abstain": False, "citations": [{"doc": "a.md", "para": 0}]},
            "expected_outcome": outcome,
            "expected_violations": violations,
            "provenance": "test",
            "source_ref": "test",
        }

    @staticmethod
    def _grade(fixture_id, **overrides):
        grade = {
            "id": fixture_id,
            "required_points_complete": True,
            "source_action_qualifier_fidelity": True,
            "language_matches_query": True,
            "correct_abstention": True,
            "citations_valid": True,
            "unsupported_claims": [],
            "reason": "independent structured judge result",
        }
        grade.update(overrides)
        return grade


if __name__ == "__main__":
    unittest.main()
