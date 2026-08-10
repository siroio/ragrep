import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

from validate_cases import main, validate_case_sets, write_retrieval_cases


class ValidateCasesTests(unittest.TestCase):
    def make_cases(self, root):
        doc = root / "Manual" / "a.md"
        doc.parent.mkdir()
        doc.write_text("# A\n\n## Requirements\n\nFact.\n", encoding="utf-8")
        return {
            "id": "dev-001", "query": "Question?", "answer": "Fact.",
            "required_points": ["Fact"],
            "evidence": [{"doc": "Manual/a.md", "heading": "Requirements", "para": 2}],
            "type": "direct", "answerable": True,
        }, {
            "id": "holdout-001", "query": "Missing?", "answer": "",
            "required_points": [], "evidence": [],
            "type": "unanswerable", "answerable": False,
        }

    def test_validates_master_cases_and_exports_retrieval_jsonl(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)

            validate_case_sets(
                [answerable], [unanswerable], root,
                expected_split_counts=(1, 1),
                expected_type_counts={"direct": 1, "unanswerable": 1},
            )
            output = root / "retrieval.jsonl"
            count = write_retrieval_cases([answerable, unanswerable], output)

            self.assertEqual(count, 1)
            self.assertEqual(
                json.loads(output.read_text(encoding="utf-8")),
                {"query": "Question?", "doc": "Manual/a.md", "para": 2},
            )

    def test_rejects_duplicate_ids(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            unanswerable["id"] = answerable["id"]

            with self.assertRaisesRegex(ValueError, "dev-001"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_missing_evidence_document(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["evidence"][0]["doc"] = "Manual/missing.md"

            with self.assertRaisesRegex(ValueError, "dev-001"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_missing_evidence_heading(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["evidence"][0]["heading"] = "Absent"

            with self.assertRaisesRegex(ValueError, "dev-001"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_negative_paragraph_number(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["evidence"][0]["para"] = -1

            with self.assertRaisesRegex(ValueError, "dev-001"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_answerable_case_without_evidence(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["evidence"] = []

            with self.assertRaisesRegex(ValueError, "dev-001"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_unanswerable_case_with_answer(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            unanswerable["answer"] = "Not allowed"

            with self.assertRaisesRegex(ValueError, "holdout-001"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_wrong_split_counts(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, _ = self.make_cases(root)

            with self.assertRaisesRegex(ValueError, "development=1"):
                validate_case_sets([answerable], [], root, (0, 1), {"direct": 1})

    def test_rejects_wrong_type_counts(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)

            with self.assertRaisesRegex(ValueError, "direct.*1"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 2, "unanswerable": 0})

    def test_matches_normalized_visible_heading_text(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            (root / "Manual" / "a.md").write_text("# A\n\n## <em>Requirements</em>\n\nFact.\n", encoding="utf-8")

            validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_heading_that_only_matches_after_removing_literal_underscore(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            (root / "Manual" / "a.md").write_text("# A\n\n## A_B\n\nFact.\n", encoding="utf-8")
            answerable["evidence"][0]["heading"] = "AB"

            with self.assertRaisesRegex(ValueError, "dev-001"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_non_string_answer_with_value_error(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["answer"] = 1

            with self.assertRaisesRegex(ValueError, "dev-001.*answer"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_non_string_id_with_value_error(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["id"] = ["dev-001"]

            with self.assertRaisesRegex(ValueError, "id"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_rejects_root_relative_windows_evidence_path(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["evidence"][0]["doc"] = "\\outside.md"

            with self.assertRaisesRegex(ValueError, "dev-001.*invalid evidence document"):
                validate_case_sets([answerable], [unanswerable], root, (1, 1), {"direct": 1, "unanswerable": 1})

    def test_cli_reports_malformed_field_as_value_error(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            answerable, unanswerable = self.make_cases(root)
            answerable["answer"] = 1
            development = root / "development.jsonl"
            holdout = root / "holdout.jsonl"
            development.write_text(json.dumps(answerable) + "\n", encoding="utf-8")
            holdout.write_text(json.dumps(unanswerable) + "\n", encoding="utf-8")
            stderr = io.StringIO()

            with contextlib.redirect_stderr(stderr):
                result = main(["--corpus", str(root), "--development", str(development), "--holdout", str(holdout), "--export", str(root / "out.jsonl")])

            self.assertEqual(result, 1)
            self.assertIn("answer", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
