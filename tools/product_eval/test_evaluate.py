import json
import os
import sys
import tempfile
import unittest
import contextlib
import io
from types import SimpleNamespace
from unittest import mock
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import evaluate


class EvaluateTests(unittest.TestCase):
    def test_score_multi_target_para_zero_and_dedup(self):
        case = {"id": "a", "query": "q", "relevant": [{"doc": "a.md", "para": 0}, {"doc": "b.md"}]}
        hits = [
            {"doc": "a.md", "para": 0, "snippet": "x"},
            {"doc": "a.md", "para": 0, "snippet": "x"},
            {"doc": "b.md", "para": 3, "snippet": "y"},
        ]
        got = evaluate.score_case(case, hits)
        self.assertEqual(got["evidence_recall"], 1.0)
        self.assertTrue(got["all_evidence_hit"])
        self.assertEqual(got["reciprocal_rank"], 1.0)

    def test_score_partial_target_has_half_recall_and_rank_two(self):
        case = {"id": "a", "query": "q", "relevant": [{"doc": "a.md", "para": 0}, {"doc": "b.md"}]}
        got = evaluate.score_case(case, [{"doc": "x.md", "para": 0, "snippet": "x"}, {"doc": "a.md", "para": 0, "snippet": "a"}])
        self.assertEqual(got["evidence_recall"], 0.5)
        self.assertFalse(got["all_evidence_hit"])
        self.assertEqual(got["reciprocal_rank"], 0.5)

    def test_no_answer_aggregate_has_null_positive_metrics(self):
        case = {"id": "n", "query": "q", "answerable": False, "relevant": []}
        run = {"id": "n", "mode": "text", "elapsed_ms": 1, "hit_count": 0, "stdout_bytes": 0, "snippet_chars": 0, "hits": []}
        aggregate = evaluate._aggregate([run], [case])
        self.assertIsNone(aggregate["text"]["evidence_recall"])
        self.assertEqual(aggregate["text"]["nonempty_candidate_rate"], 0)

    def test_invoke_rejects_boundary_failures_and_uses_query_separator(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td); (root / "a.md").write_text("x", encoding="utf-8")
            good = b'[{"doc":"a.md","para":0,"snippet":"x"}]'
            cases = [(1, b"", "search exit"), (2, good, "search exit"), (0, b"bad", "malformed"), (0, b'[{"doc":"a.md","para":0,"snippet":"x","stale":true}]', "invalid"), (0, b'[{"doc":"a.md","para":0,"snippet":"x"},{"doc":"a.md","para":0,"snippet":"x"}]', "duplicate")]
            for code, stdout, expected in cases:
                with self.subTest(code=code, stdout=stdout):
                    with mock.patch.object(evaluate.subprocess, "run", return_value=SimpleNamespace(returncode=code, stdout=stdout, stderr=b"raw-stderr")):
                        with self.assertRaisesRegex(Exception, expected):
                            evaluate._invoke(["fake"], ["search", "-leading"], root, 1, 1)
            with mock.patch.object(evaluate.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=good, stderr=b"")) as call:
                evaluate._invoke(["fake"], ["search", "-leading"], root, 1, 1)
                self.assertIn("--", call.call_args.args[0])

    def test_invoke_rejects_too_many_hits_and_timeout(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td); (root / "a.md").write_text("x", encoding="utf-8")
            many = b'[{"doc":"a.md","para":0,"snippet":"x"},{"doc":"a.md","para":1,"snippet":"x"}]'
            with mock.patch.object(evaluate.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=many, stderr=b"")):
                with self.assertRaisesRegex(evaluate.InvocationFailure, "invalid hit list"):
                    evaluate._invoke(["fake"], ["q"], root, 1, 1)
            with mock.patch.object(evaluate.subprocess, "run", side_effect=TimeoutError("timed out")):
                with self.assertRaises(TimeoutError):
                    evaluate._invoke(["fake"], ["q"], root, 1, 1)

    def test_run_pairs_cases_and_reverses_modes_each_repeat(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td); (root / "a.md").write_text("x", encoding="utf-8")
            cases = [{"id": "a", "query": "x", "relevant": [{"doc": "a.md"}]}]
            args = SimpleNamespace(root=str(root), ragrep="fake", db="db", modes="text,hybrid", k=1, repeats=2, timeout=1)
            seen = []
            hit = [{"doc": "a.md", "para": 0, "snippet": "x"}]
            def fake_invoke(exe, command, root_path, timeout, k):
                seen.append(command[command.index("--mode") + 1])
                return 1.0, SimpleNamespace(stdout=b"x"), hit, command
            with mock.patch.object(evaluate, "_invoke", side_effect=fake_invoke):
                evaluate.run(args, cases)
            self.assertEqual(seen, ["text", "hybrid", "text", "hybrid", "hybrid", "text"])

    def test_timeout_diagnostic_serializes_partial_bytes_and_context(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "a.md").write_text("x", encoding="utf-8")
            db = root / "db"; db.write_text("", encoding="utf-8")
            fake = root / "fake.py"; fake.write_text("print('unused')", encoding="utf-8")
            cases = root / "cases.jsonl"; cases.write_text(json.dumps({"id":"a","query":"x","relevant":[{"doc":"a.md"}]}) + "\n", encoding="utf-8")
            out = root / "report.json"
            timeout = evaluate.subprocess.TimeoutExpired([str(fake)], 1, output=b"partial", stderr=b"waiting")
            stderr = io.StringIO()
            with mock.patch.object(evaluate.subprocess, "run", side_effect=timeout), contextlib.redirect_stderr(stderr):
                self.assertEqual(evaluate.main(["--ragrep", str(fake), "--db", str(db), "--root", str(root), "--cases", str(cases), "--output", str(out), "--modes", "text"]), 1)
            self.assertIn("timed out", stderr.getvalue())
            diagnostic = json.loads((root / "report.json.error.json").read_text(encoding="utf-8"))
            self.assertEqual(diagnostic["stderr"], "waiting")
            self.assertEqual(diagnostic["context"]["mode"], "text")
            self.assertEqual(diagnostic["completed_runs"], [])

    def test_score_no_answer_and_invalid_cases(self):
        self.assertEqual(evaluate.score_case({"id": "n", "query": "q", "answerable": False}, [])["nonempty_candidate"], False)
        with self.assertRaises(ValueError):
            evaluate.load_cases(["{\"query\": \" \"}"])
        with self.assertRaises(ValueError):
            evaluate.load_cases([json.dumps({"id": "x", "query": "q", "answerable": False, "relevant": [{"doc": "a"}]})])

    @unittest.skipUnless(Path(sys.executable).is_file(), "requires an executable Python interpreter")
    def test_run_cli_records_modes_and_preserves_output_on_failure(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "a.md").write_text("hello", encoding="utf-8")
            fake = root / "fake.py"
            fake.write_text("import json; print(json.dumps([{'doc':'a.md','para':0,'snippet':'hello'}]))", encoding="utf-8")
            (root / "db").write_text("", encoding="utf-8")
            cases = root / "cases.jsonl"
            cases.write_text(json.dumps({"id":"a","query":"hello","relevant":[{"doc":"a.md","para":0}]}) + "\n", encoding="utf-8")
            out = root / "out.json"
            rc = evaluate.main(["--ragrep", str(fake), "--db", str(root / "db"), "--root", str(root), "--cases", str(cases), "--output", str(out), "--repeats", "1", "--modes", "text"])
            self.assertEqual(rc, 0)
            report = json.loads(out.read_text(encoding="utf-8"))
            self.assertEqual(len(report["runs"]), 1)
            self.assertEqual(report["runs"][0]["mode"], "text")

    def test_existing_output_is_rejected_and_unchanged(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td); (root / "a.md").write_text("x", encoding="utf-8")
            cases = root / "cases.jsonl"
            cases.write_text(json.dumps({"id":"a","query":"x","relevant":[{"doc":"a.md"}]}) + "\n", encoding="utf-8")
            out = root / "out.json"; out.write_text("keep", encoding="utf-8")
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr):
                self.assertNotEqual(evaluate.main(["--ragrep", "missing", "--db", "db", "--root", str(root), "--cases", str(cases), "--output", str(out), "--modes", "text"]), 0)
            self.assertIn("output already exists", stderr.getvalue())
            self.assertEqual(out.read_text(encoding="utf-8"), "keep")

    def test_validation_skips_blank_lines_and_rejects_bad_metadata(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td); (root / "a.md").write_text("x", encoding="utf-8")
            lines = ["", json.dumps({"id":"a","query":"x","relevant":[{"doc":"a.md"}],"category":"docs","language":"ja"})]
            self.assertEqual(len(evaluate.load_cases(lines, root)), 1)
            for bad in ({"category": 1}, {"language": False}, {"relevant": {"doc":"a.md"}}):
                item = {"id":"b","query":"x","relevant":[{"doc":"a.md"}], **bad}
                with self.assertRaises(ValueError): evaluate.load_cases([json.dumps(item)], root)


if __name__ == "__main__":
    unittest.main()
