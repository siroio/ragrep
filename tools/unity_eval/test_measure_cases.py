import json
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

import measure_cases


class MeasureCasesTests(unittest.TestCase):
    def write_jsonl(self, path: Path, rows: list[dict]) -> Path:
        path.write_text(
            "".join(json.dumps(row, ensure_ascii=False) + "\n" for row in rows),
            encoding="utf-8",
        )
        return path

    def test_load_questions_strips_gold(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            development = self.write_jsonl(
                root / "development.jsonl",
                [{
                    "id": "dev-001", "query": "開発質問", "answer": "秘密",
                    "required_points": ["秘密"], "evidence": [{"doc": "private/a.md"}],
                    "type": "direct", "domain": "ui",
                }],
            )
            holdout = self.write_jsonl(
                root / "holdout-questions.jsonl",
                [{"id": "hold-001", "query": "保持質問"}],
            )

            questions = measure_cases.load_questions(development, holdout)

            self.assertEqual(questions, [
                {"id": "dev-001", "query": "開発質問", "split": "development"},
                {"id": "hold-001", "query": "保持質問", "split": "holdout"},
            ])
            serialized = json.dumps(questions, ensure_ascii=False)
            for secret in ("answer", "required_points", "evidence", "private/a.md", "秘密"):
                self.assertNotIn(secret, serialized)

    def test_load_questions_rejects_invalid_public_rows(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            development = self.write_jsonl(
                root / "development.jsonl", [{"id": "same", "query": "dev"}]
            )
            for row in (
                {"id": "same", "query": "hold"},
                {"id": "hold", "query": " "},
                {"id": "hold", "query": "hold", "answer": "leak"},
            ):
                with self.subTest(row=row):
                    holdout = self.write_jsonl(root / "holdout.jsonl", [row])
                    with self.assertRaises(ValueError):
                        measure_cases.load_questions(development, holdout)

    def test_build_dispatch_is_complete_and_deterministic(self):
        questions = [
            {"id": f"q-{number:03}", "query": f"question {number}", "split": "development"}
            for number in range(100)
        ]

        first = measure_cases.build_dispatch(questions, repetitions=5, seed=42)
        second = measure_cases.build_dispatch(questions, repetitions=5, seed=42)
        different = measure_cases.build_dispatch(questions, repetitions=5, seed=43)

        self.assertEqual(len(first), 1500)
        self.assertEqual(len({row["run_key"] for row in first}), 1500)
        self.assertEqual(first, second)
        self.assertNotEqual(
            [row["id"] for row in first], [row["id"] for row in different]
        )
        for condition in (
            "c0_no_skill_no_ragrep", "c1_ragrep_no_skill", "c2_skill_ragrep"
        ):
            for repetition in range(1, 6):
                rows = [
                    row for row in first
                    if row["condition"] == condition and row["repetition"] == repetition
                ]
                self.assertEqual([row["order"] for row in rows], list(range(1, 101)))
                self.assertEqual({row["id"] for row in rows}, {q["id"] for q in questions})
                self.assertTrue(all(
                    row["run_key"] == f'{condition}/r{repetition}/{row["id"]}'
                    for row in rows
                ))
                self.assertTrue(all(
                    row["query"] == f"question {int(row['id'][2:])}" for row in rows
                ))

    def test_prepare_run_publishes_isolated_reproducible_files(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            development = self.write_jsonl(root / "development.jsonl", [
                {"id": "direct", "query": "d", "answer": "secret"},
                {"id": "complex", "query": "c", "answer": "secret"},
                {"id": "unknown", "query": "u", "answer": ""},
            ])
            holdout = self.write_jsonl(
                root / "holdout.jsonl", [{"id": "hold", "query": "h"}]
            )
            sources = {}
            for name in ("frozen_manifest", "holdout_gold", "validator", "skill", "db", "binary"):
                sources[name] = root / name
                sources[name].write_bytes(name.encode())
            output = root / "run"

            manifest = measure_cases.prepare_run(
                development=development,
                holdout_questions=holdout,
                output=output,
                pilot_ids=("direct", "complex", "unknown"),
                seed=7,
                generated_at=datetime(2026, 8, 12, 10, tzinfo=timezone.utc),
                prompt_hashes={
                    "c0_no_skill_no_ragrep": "0" * 64,
                    "c1_ragrep_no_skill": "1" * 64,
                    "c2_skill_ragrep": "2" * 64,
                },
                **sources,
            )

            self.assertEqual(manifest["generated_at"], "2026-08-12T10:00:00Z")
            self.assertEqual(manifest["run_count"], 60)
            self.assertEqual(manifest["pilot_run_count"], 9)
            self.assertEqual(manifest["model"], "gpt-5.6-sol")
            self.assertEqual(manifest["reasoning"], "medium")
            self.assertEqual(manifest["timeout_seconds"], 600)
            self.assertEqual(manifest["concurrency"], 8)
            self.assertEqual(manifest["repetitions"], 5)
            self.assertIn("sha256", manifest)
            self.assertEqual(
                manifest["sha256"]["binary"], measure_cases.sha256_file(sources["binary"])
            )
            public = (output / "questions-only.jsonl").read_text(encoding="utf-8")
            self.assertNotIn("secret", public)
            self.assertTrue(all("question_sha256" in json.loads(line) for line in public.splitlines()))
            self.assertEqual(len((output / "dispatch.jsonl").read_text().splitlines()), 60)
            self.assertEqual(len((output / "pilot-dispatch.jsonl").read_text().splitlines()), 9)
            rendered = (output / "manifest.json").read_text(encoding="utf-8")
            self.assertEqual(rendered, json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n")

            with self.assertRaisesRegex(ValueError, "already exists"):
                measure_cases.prepare_run(
                    development=development, holdout_questions=holdout, output=output,
                    pilot_ids=("direct", "complex", "unknown"), seed=7,
                    generated_at=datetime.now(timezone.utc),
                    prompt_hashes=manifest["prompt_hashes"], **sources,
                )

    def test_prepare_run_rejects_naive_timestamp_before_writing(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            files = {}
            for name in (
                "development", "holdout_questions", "frozen_manifest", "holdout_gold",
                "validator", "skill", "db", "binary",
            ):
                files[name] = root / name
                files[name].write_text('{"id":"q","query":"x"}\n')
            output = root / "run"
            with self.assertRaisesRegex(ValueError, "timezone"):
                measure_cases.prepare_run(
                    output=output, pilot_ids=("q", "q", "q"), seed=1,
                    generated_at=datetime(2026, 8, 12),
                    prompt_hashes={
                        "c0_no_skill_no_ragrep": "0" * 64,
                        "c1_ragrep_no_skill": "1" * 64,
                        "c2_skill_ragrep": "2" * 64,
                    },
                    **files,
                )
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
