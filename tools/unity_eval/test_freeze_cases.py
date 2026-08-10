import contextlib
import io
import json
from datetime import datetime, timezone
from pathlib import Path
import tempfile
import unittest
from unittest import mock

from freeze_cases import (
    freeze_case_set,
    main,
    sha256_file,
    validate_allocation,
    validate_reviews,
    verify_holdout_questions,
)


class FreezeCasesTests(unittest.TestCase):
    def make_inputs(self, root: Path):
        corpus = root / "corpus"
        document = corpus / "Manual" / "a.md"
        document.parent.mkdir(parents=True)
        document.write_text("# A\n\n## Requirements\n\nFact.\n", encoding="utf-8")
        manifest = root / "corpus-manifest.json"
        manifest.write_text(
            json.dumps({"unity_version": "6000.3.11f1"}) + "\n",
            encoding="utf-8",
        )
        development_case = {
            "id": "dev-001",
            "query": "Question?",
            "answer": "Fact.",
            "required_points": ["Fact"],
            "evidence": [
                {"doc": "Manual/a.md", "heading": "Requirements", "para": 2}
            ],
            "type": "direct",
            "answerable": True,
            "domain": "rendering",
            "family": "static-batching-runtime",
            "unanswerable_kind": None,
        }
        holdout_case = {
            "id": "holdout-001",
            "query": "Missing?",
            "answer": "",
            "required_points": [],
            "evidence": [],
            "type": "unanswerable",
            "answerable": False,
            "domain": "scripting_concepts",
            "family": "runtime-api-only-question",
            "unanswerable_kind": "corpus_outside",
        }
        development = root / "development.jsonl"
        holdout = root / "holdout-gold.jsonl"
        development.write_text(
            json.dumps(development_case, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        holdout.write_text(
            json.dumps(holdout_case, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        allocation = root / "allocation.json"
        allocation.write_text(
            json.dumps(
                [
                    {
                        "id": development_case["id"],
                        "split": "development",
                        "type": development_case["type"],
                        "domain": development_case["domain"],
                        "unanswerable_kind": development_case["unanswerable_kind"],
                        "family": development_case["family"],
                        "status": "approved",
                    },
                    {
                        "id": holdout_case["id"],
                        "split": "holdout",
                        "type": holdout_case["type"],
                        "domain": holdout_case["domain"],
                        "unanswerable_kind": holdout_case["unanswerable_kind"],
                        "family": holdout_case["family"],
                        "status": "approved",
                    },
                ],
                ensure_ascii=False,
            )
            + "\n",
            encoding="utf-8",
        )
        review = root / "review.jsonl"
        review.write_text(
            "".join(
                json.dumps(
                    {"id": case["id"], "round": 1, "verdict": "approved"},
                    ensure_ascii=False,
                )
                + "\n"
                for case in (development_case, holdout_case)
            ),
            encoding="utf-8",
        )
        validation_kwargs = {
            "expected_split_counts": (1, 1),
            "expected_type_counts": {"direct": 1, "unanswerable": 1},
            "expected_split_type_counts": ({"direct": 1}, {"unanswerable": 1}),
            "expected_domain_counts": {"rendering": 1, "scripting_concepts": 1},
            "expected_split_domain_counts": (
                {"rendering": 1},
                {"scripting_concepts": 1},
            ),
            "expected_unanswerable_kind_counts": {"corpus_outside": 1},
            "expected_split_unanswerable_kind_counts": (
                {},
                {"corpus_outside": 1},
            ),
        }
        return {
            "corpus_root": corpus,
            "corpus_manifest": manifest,
            "development": development,
            "holdout_gold": holdout,
            "allocation": allocation,
            "review": review,
            "development_case": development_case,
            "holdout_case": holdout_case,
            "validation_kwargs": validation_kwargs,
        }

    def freeze(self, inputs, public_output, private_output, generated_at=None):
        return freeze_case_set(
            **{
                key: inputs[key]
                for key in (
                    "corpus_root",
                    "corpus_manifest",
                    "development",
                    "holdout_gold",
                    "allocation",
                    "review",
                )
            },
            public_output=public_output,
            private_output=private_output,
            doc_prefix="prepared-corpus",
            generated_at=generated_at
            or datetime(2026, 8, 11, 18, 30, tzinfo=timezone.utc),
            validation_kwargs=inputs["validation_kwargs"],
        )

    def test_freezes_public_and_private_versioned_artifacts(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            public_output = root / "public" / "v1"
            private_output = root / "private" / "v1"

            manifest = self.freeze(inputs, public_output, private_output)

            self.assertEqual(
                {path.name for path in public_output.iterdir()},
                {
                    "development.jsonl",
                    "holdout-questions.jsonl",
                    "retrieval.jsonl",
                    "manifest.json",
                },
            )
            self.assertEqual(
                {path.name for path in private_output.iterdir()},
                {"holdout-gold.jsonl", "allocation.json", "review.jsonl"},
            )
            self.assertEqual(
                (public_output / "development.jsonl").read_bytes(),
                inputs["development"].read_bytes(),
            )
            questions = json.loads(
                (public_output / "holdout-questions.jsonl").read_text(encoding="utf-8")
            )
            self.assertEqual(questions, {"id": "holdout-001", "query": "Missing?"})
            retrieval = json.loads(
                (public_output / "retrieval.jsonl").read_text(encoding="utf-8")
            )
            self.assertEqual(retrieval["doc"], "prepared-corpus/Manual/a.md")
            self.assertEqual(manifest["generated_at"], "2026-08-11T18:30:00Z")
            self.assertEqual(manifest["counts"]["answerable"], 1)
            self.assertEqual(manifest["counts"]["retrieval_entries"], 1)
            for key, path in {
                "development": public_output / "development.jsonl",
                "holdout_questions": public_output / "holdout-questions.jsonl",
                "holdout_gold": private_output / "holdout-gold.jsonl",
                "allocation": private_output / "allocation.json",
                "review": private_output / "review.jsonl",
            }.items():
                self.assertEqual(manifest["sha256"][key], sha256_file(path))
            self.assertEqual(
                json.loads((public_output / "manifest.json").read_text(encoding="utf-8")),
                manifest,
            )

    def test_refuses_existing_public_output(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            public_output = root / "public" / "v1"
            public_output.mkdir(parents=True)
            sentinel = public_output / "keep.txt"
            sentinel.write_text("keep", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "already exists"):
                self.freeze(inputs, public_output, root / "private" / "v1")

            self.assertEqual(sentinel.read_text(encoding="utf-8"), "keep")
            self.assertFalse((root / "private" / "v1").exists())

    def test_refuses_existing_private_output(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            private_output = root / "private" / "v1"
            private_output.mkdir(parents=True)
            sentinel = private_output / "keep.txt"
            sentinel.write_text("keep", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "already exists"):
                self.freeze(inputs, root / "public" / "v1", private_output)

            self.assertEqual(sentinel.read_text(encoding="utf-8"), "keep")
            self.assertFalse((root / "public" / "v1").exists())

    def test_removes_staging_directories_after_failure(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            public_output = root / "public" / "v1"
            private_output = root / "private" / "v1"
            real_rename = Path.rename

            def fail_public_rename(path, target):
                if target == public_output:
                    raise OSError("simulated public publication failure")
                return real_rename(path, target)

            with mock.patch.object(Path, "rename", autospec=True, side_effect=fail_public_rename):
                with self.assertRaisesRegex(OSError, "simulated"):
                    self.freeze(inputs, public_output, private_output)

            self.assertFalse(public_output.exists())
            self.assertFalse(private_output.exists())
            self.assertEqual(list((root / "public").glob(".v1.*")), [])
            self.assertEqual(list((root / "private").glob(".v1.*")), [])

    def test_removes_first_staging_directory_if_second_creation_fails(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            public_output = root / "public" / "v1"
            private_output = root / "private" / "v1"
            real_mkdtemp = tempfile.mkdtemp
            calls = 0

            def fail_second_mkdtemp(*args, **kwargs):
                nonlocal calls
                calls += 1
                if calls == 2:
                    raise OSError("simulated private staging failure")
                return real_mkdtemp(*args, **kwargs)

            with mock.patch("freeze_cases.tempfile.mkdtemp", side_effect=fail_second_mkdtemp):
                with self.assertRaisesRegex(OSError, "simulated"):
                    self.freeze(inputs, public_output, private_output)

            self.assertEqual(list((root / "public").glob(".v1.*")), [])
            self.assertEqual(list((root / "private").glob(".v1.*")), [])

    def test_rejects_holdout_question_id_or_query_mismatch(self):
        with tempfile.TemporaryDirectory() as raw:
            questions = Path(raw) / "questions.jsonl"
            questions.write_text(
                json.dumps({"id": "holdout-001", "query": "Changed"}) + "\n",
                encoding="utf-8",
            )

            with self.assertRaisesRegex(ValueError, "holdout-001"):
                verify_holdout_questions(
                    [{"id": "holdout-001", "query": "Original"}], questions
                )

    def test_rejects_invalid_prefix_before_creating_output_parents(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            public_output = root / "public" / "v1"
            private_output = root / "private" / "v1"

            with self.assertRaisesRegex(ValueError, "prefix"):
                freeze_case_set(
                    **{
                        key: inputs[key]
                        for key in (
                            "corpus_root",
                            "corpus_manifest",
                            "development",
                            "holdout_gold",
                            "allocation",
                            "review",
                        )
                    },
                    public_output=public_output,
                    private_output=private_output,
                    doc_prefix="../escape",
                    generated_at=datetime(2026, 8, 11, tzinfo=timezone.utc),
                    validation_kwargs=inputs["validation_kwargs"],
                )

            self.assertFalse(public_output.parent.exists())
            self.assertFalse(private_output.parent.exists())

    def test_rejects_allocation_case_mismatch(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            slots = json.loads(inputs["allocation"].read_text(encoding="utf-8"))
            slots[0]["family"] = "wrong-family"
            inputs["allocation"].write_text(json.dumps(slots) + "\n", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "dev-001.*family"):
                validate_allocation(
                    inputs["allocation"],
                    [inputs["development_case"]],
                    [inputs["holdout_case"]],
                )

    def test_rejects_missing_or_nonapproved_latest_review(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)
            cases = [inputs["development_case"], inputs["holdout_case"]]
            for records, message in (
                (
                    [{"id": "dev-001", "round": 1, "verdict": "approved"}],
                    "holdout-001",
                ),
                (
                    [
                        {"id": "dev-001", "round": 1, "verdict": "approved"},
                        {"id": "dev-001", "round": 2, "verdict": "changes_requested"},
                        {"id": "holdout-001", "round": 1, "verdict": "approved"},
                    ],
                    "dev-001",
                ),
            ):
                with self.subTest(message=message):
                    inputs["review"].write_text(
                        "".join(json.dumps(record) + "\n" for record in records),
                        encoding="utf-8",
                    )
                    with self.assertRaisesRegex(ValueError, message):
                        validate_reviews(inputs["review"], cases)

    def test_manifest_is_deterministic_for_fixed_timestamp(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            inputs = self.make_inputs(root)

            first = self.freeze(inputs, root / "public-v1", root / "private-v1")
            second = self.freeze(inputs, root / "public-v2", root / "private-v2")

            self.assertEqual(first, second)
            self.assertEqual(
                (root / "public-v1" / "manifest.json").read_bytes(),
                (root / "public-v2" / "manifest.json").read_bytes(),
            )

    def test_cli_requires_generated_at_timezone(self):
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            result = main(
                [
                    "--corpus", "corpus",
                    "--corpus-manifest", "corpus-manifest.json",
                    "--development", "development.jsonl",
                    "--holdout-gold", "holdout-gold.jsonl",
                    "--allocation", "allocation.json",
                    "--review", "review.jsonl",
                    "--public-output", "public",
                    "--private-output", "private",
                    "--doc-prefix", "prepared-corpus",
                    "--generated-at", "2026-08-11T18:30:00",
                ]
            )

        self.assertEqual(result, 1)
        self.assertEqual(stderr.getvalue(), "error: --generated-at requires a timezone\n")

    def test_cli_prints_exact_summary(self):
        public_output = Path(r"D:\Works\ragrep-eval\unity-6000.3.11f1-r2\cases\v1")
        private_output = Path(r"D:\Works\ragrep-eval-private\unity-6000.3.11f1\v1")
        frozen = {
            "counts": {
                "development": 30,
                "holdout": 70,
                "answerable": 90,
                "retrieval_entries": 115,
            }
        }
        stdout = io.StringIO()
        with mock.patch("freeze_cases.freeze_case_set", return_value=frozen):
            with contextlib.redirect_stdout(stdout):
                result = main(
                    [
                        "--corpus", "corpus",
                        "--corpus-manifest", "corpus-manifest.json",
                        "--development", "development.jsonl",
                        "--holdout-gold", "holdout-gold.jsonl",
                        "--allocation", "allocation.json",
                        "--review", "review.jsonl",
                        "--public-output", str(public_output),
                        "--private-output", str(private_output),
                        "--doc-prefix", "prepared-corpus",
                        "--generated-at", "2026-08-11T18:30:00+09:00",
                    ]
                )

        self.assertEqual(result, 0)
        self.assertEqual(
            stdout.getvalue(),
            "development=30 holdout=70 answerable=90 retrieval=115 "
            f"public={public_output} private={private_output}\n",
        )


if __name__ == "__main__":
    unittest.main()
