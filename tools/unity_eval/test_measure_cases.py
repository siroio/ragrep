import contextlib
import io
import json
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest import mock

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

    def test_build_dispatch_round_robins_condition_and_repetition_queues(self):
        questions = [
            {"id": f"q-{number}", "query": str(number), "split": "development"}
            for number in range(2)
        ]

        dispatch = measure_cases.build_dispatch(questions, repetitions=2, seed=42)

        self.assertEqual(
            [(row["condition"], row["repetition"]) for row in dispatch[:6]],
            [
                ("c0_no_skill_no_ragrep", 1),
                ("c1_ragrep_no_skill", 1),
                ("c2_skill_ragrep", 1),
                ("c0_no_skill_no_ragrep", 2),
                ("c1_ragrep_no_skill", 2),
                ("c2_skill_ragrep", 2),
            ],
        )

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
            for name in ("frozen_manifest", "holdout_gold", "validator", "skill", "db", "binary", "wrapper"):
                sources[name] = root / name
                sources[name].write_bytes(name.encode())
            prompts = {}
            for condition in measure_cases.CONDITIONS:
                prompts[condition] = root / f"{condition}.prompt"
                prompts[condition].write_text(condition, encoding="utf-8")
            output = root / "run"
            controller_inputs = root / "private" / "controller-inputs.json"

            manifest = measure_cases.prepare_run(
                development=development,
                holdout_questions=holdout,
                output=output,
                pilot_ids=("direct", "complex", "unknown"),
                seed=7,
                generated_at=datetime(2026, 8, 12, 10, tzinfo=timezone.utc),
                prompt_sources=prompts,
                controller_inputs=controller_inputs,
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
            self.assertEqual(
                manifest["prompt_hashes"]["c1_ragrep_no_skill"],
                measure_cases.sha256_file(prompts["c1_ragrep_no_skill"]),
            )
            self.assertEqual(manifest["wrapper_path"], str(sources["wrapper"].resolve()))
            self.assertTrue(controller_inputs.is_file())
            self.assertNotIn(str(sources["holdout_gold"].resolve()), json.dumps(manifest))
            public = (output / "questions-only.jsonl").read_text(encoding="utf-8")
            self.assertNotIn("secret", public)
            self.assertTrue(all("question_sha256" in json.loads(line) for line in public.splitlines()))
            self.assertEqual(len((output / "dispatch.jsonl").read_text().splitlines()), 60)
            self.assertEqual(len((output / "pilot-dispatch.jsonl").read_text().splitlines()), 9)
            pilot_rows = [json.loads(line) for line in (output / "pilot-dispatch.jsonl").read_text().splitlines()]
            self.assertEqual(
                [row["condition"] for row in pilot_rows[:6]],
                [
                    "c0_no_skill_no_ragrep", "c1_ragrep_no_skill", "c2_skill_ragrep",
                    "c0_no_skill_no_ragrep", "c1_ragrep_no_skill", "c2_skill_ragrep",
                ],
            )
            rendered = (output / "manifest.json").read_text(encoding="utf-8")
            self.assertEqual(rendered, json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n")

            with self.assertRaisesRegex(ValueError, "already exists"):
                measure_cases.prepare_run(
                    development=development, holdout_questions=holdout, output=output,
                    pilot_ids=("direct", "complex", "unknown"), seed=7,
                    generated_at=datetime.now(timezone.utc),
                    prompt_sources=prompts, controller_inputs=controller_inputs, **sources,
                )

    def test_prepare_run_rejects_naive_timestamp_before_writing(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            files = {}
            for name in (
                "development", "holdout_questions", "frozen_manifest", "holdout_gold",
                "validator", "skill", "db", "binary", "wrapper",
            ):
                files[name] = root / name
                files[name].write_text('{"id":"q","query":"x"}\n')
            output = root / "run"
            with self.assertRaisesRegex(ValueError, "timezone"):
                measure_cases.prepare_run(
                    output=output, pilot_ids=("q", "q", "q"), seed=1,
                    generated_at=datetime(2026, 8, 12),
                    prompt_sources={condition: files["validator"] for condition in measure_cases.CONDITIONS},
                    controller_inputs=root / "controller-inputs.json",
                    **files,
                )
            self.assertFalse(output.exists())


class WorkerOutputTests(unittest.TestCase):
    def valid(self):
        return {
            "id": "unity-manual-001",
            "answerable": True,
            "answer": "回答",
            "evidence": [{"doc": "Manual/a.md", "para": 1}],
            "searches": ["query"],
            "retrievals": [{
                "query": "query", "mode": "hybrid",
                "hits": [{"rank": 1, "doc": "Manual/a.md", "para": 1}],
            }],
            "notes": "",
        }

    def parse(self, value, condition="c2_skill_ragrep"):
        text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
        return measure_cases.parse_worker_output(text, "unity-manual-001", condition)

    def test_parse_worker_output_accepts_exact_schema(self):
        self.assertEqual(self.parse(self.valid()), self.valid())

    def test_parse_worker_output_rejects_malformed_variants(self):
        variants = []
        variants.extend(("```json\n{}\n```", "before {}", "{} after"))
        for key in self.valid():
            value = self.valid()
            del value[key]
            variants.append(value)
        extra = self.valid(); extra["extra"] = True; variants.append(extra)
        wrong_id = self.valid(); wrong_id["id"] = "other"; variants.append(wrong_id)
        wrong_bool = self.valid(); wrong_bool["answerable"] = 1; variants.append(wrong_bool)
        for doc in (
            r"C:\secret.md", "../secret.md", "/secret.md", r"..\secret.md",
            r"\secret.md", r"C:secret.md", r"Manual/..\secret.md",
        ):
            value = self.valid(); value["evidence"][0]["doc"] = doc; variants.append(value)
        negative_para = self.valid(); negative_para["evidence"][0]["para"] = -1; variants.append(negative_para)
        duplicate_rank = self.valid(); duplicate_rank["retrievals"][0]["hits"].append(
            {"rank": 1, "doc": "Manual/b.md", "para": 2}
        ); variants.append(duplicate_rank)
        invalid_mode = self.valid(); invalid_mode["retrievals"][0]["mode"] = "magic"; variants.append(invalid_mode)
        answer_without_evidence = self.valid(); answer_without_evidence["evidence"] = []; variants.append(answer_without_evidence)
        for value in variants:
            with self.subTest(value=value):
                with self.assertRaises((ValueError, json.JSONDecodeError)):
                    self.parse(value)

    def test_parse_worker_output_enforces_condition_rules(self):
        with self.assertRaisesRegex(ValueError, "C0"):
            self.parse(self.valid(), "c0_no_skill_no_ragrep")
        for condition in ("c1_ragrep_no_skill", "c2_skill_ragrep"):
            for field in ("searches", "retrievals"):
                value = self.valid(); value[field] = []
                with self.subTest(condition=condition, field=field):
                    with self.assertRaises(ValueError):
                        self.parse(value, condition)
        c0 = self.valid(); c0["retrievals"] = []
        self.assertEqual(self.parse(c0, "c0_no_skill_no_ragrep"), c0)
        unknown = self.valid(); unknown["answerable"] = False; unknown["evidence"] = []
        self.assertEqual(self.parse(unknown), unknown)
        errored = self.valid(); errored["evidence"] = []; errored["notes"] = "tool error"
        self.assertEqual(self.parse(errored), errored)


class TranscriptTests(unittest.TestCase):
    def write_transcript(self, path: Path, thread_id="thread-1", calls=None):
        rows = [
            {"timestamp": "2026-08-12T00:00:00Z", "type": "session_meta",
             "payload": {"id": thread_id}},
            {"timestamp": "2026-08-12T00:00:01Z", "type": "response_item",
             "payload": {"type": "message", "role": "user", "content": "ragrep-eval-private in prompt only"}},
            {"timestamp": "2026-08-12T00:00:02Z", "type": "event_msg",
             "payload": {"type": "token_count", "info": {"last_token_usage": {
                 "input_tokens": 1, "output_tokens": 2, "total_tokens": 3}}}},
        ]
        for number, (namespace, name, arguments, output) in enumerate(calls or [], start=1):
            call_id = f"call-{number}"
            rows.extend([
                {"timestamp": f"2026-08-12T00:00:{number + 2:02}Z", "type": "response_item",
                 "payload": {"type": "function_call", "call_id": call_id,
                             "namespace": namespace, "name": name,
                             "arguments": json.dumps(arguments)}},
                {"timestamp": f"2026-08-12T00:00:{number + 3:02}Z", "type": "response_item",
                 "payload": {"type": "function_call_output", "call_id": call_id,
                             "status": "completed", "output": output}},
            ])
        rows.append(
            {"timestamp": "2026-08-12T00:01:00Z", "type": "event_msg",
             "payload": {"type": "token_count", "info": {"last_token_usage": {
                 "input_tokens": 120, "cached_input_tokens": 80,
                 "output_tokens": 30, "reasoning_output_tokens": 10,
                 "total_tokens": 150}}}}
        )
        path.write_text(
            "not-json\n" + "".join(json.dumps(row) + "\n" for row in rows),
            encoding="utf-8",
        )
        return path

    def test_find_transcript_requires_one_exact_session_id(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            self.write_transcript(root / "partial.jsonl", "thread-10")
            expected = self.write_transcript(root / "exact.jsonl", "thread-1")
            self.assertEqual(measure_cases.find_transcript(root, "thread-1"), expected)
            with self.assertRaisesRegex(ValueError, "no transcript"):
                measure_cases.find_transcript(root, "missing")
            self.write_transcript(root / "duplicate.jsonl", "thread-1")
            with self.assertRaisesRegex(ValueError, "multiple"):
                measure_cases.find_transcript(root, "thread-1")

    def test_parse_transcript_keeps_last_tokens_and_bounded_tool_trace(self):
        with tempfile.TemporaryDirectory() as raw:
            path = self.write_transcript(
                Path(raw) / "session.jsonl",
                calls=[("shell", "shell_command", {"command": "wrapper ragrep search query"}, "x" * 2000)],
            )
            parsed = measure_cases.parse_transcript(path)
            self.assertEqual(parsed["thread_id"], "thread-1")
            self.assertEqual(parsed["tokens"], {
                "input_tokens": 120,
                "cached_input_tokens": 80,
                "output_tokens": 30,
                "reasoning_output_tokens": 10,
                "total_tokens": 150,
            })
            self.assertEqual(parsed["tool_counts"], {"shell__shell_command": 1})
            self.assertEqual(parsed["ragrep_search_count"], 1)
            self.assertEqual(parsed["rg_count"], 0)
            self.assertLessEqual(len(parsed["tool_calls"][0]["output_excerpt"]), 512)
            self.assertEqual(
                parsed["tool_calls"][0]["output_sha256"],
                "5c0e0ea421571c300b5df6aec0a118b5c3dc02e0683a546341d5efc689df2f58",
            )
            self.assertNotIn("ragrep-eval-private in prompt only", json.dumps(parsed))
            self.assertEqual(parsed["duration_seconds"], 60.0)

    def test_audit_transcript_enforces_condition_boundaries(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            cases = [
                ("c0_no_skill_no_ragrep", [("shell", "shell_command", {"command": "ragrep search x"}, "ok")]),
                ("c0_no_skill_no_ragrep", [("mcp__docs", "search", {"query": "x"}, "ok")]),
                ("c1_ragrep_no_skill", [("shell", "shell_command", {"command": "Get-Content skills/search/SKILL.md"}, "ok")]),
                ("c1_ragrep_no_skill", [("shell", "shell_command", {"command": "ragrep.exe search x"}, "ok")]),
                ("c2_skill_ragrep", [("shell", "shell_command", {"command": "wrapper ragrep search x"}, "holdout-gold.jsonl")]),
            ]
            for number, (condition, calls) in enumerate(cases):
                path = self.write_transcript(root / f"bad-{number}.jsonl", calls=calls)
                violations = measure_cases.audit_transcript(
                    measure_cases.parse_transcript(path), condition,
                    frozen_skill_path=r"D:\fixed\skills\search\SKILL.md",
                    frozen_wrapper_path=r"D:\run\wrapper.ps1",
                )
                self.assertTrue(violations, (condition, calls))

            good_c1 = self.write_transcript(
                root / "good-c1.jsonl",
                calls=[("shell", "shell_command", {"command": r"D:\run\wrapper.ps1 ragrep search x"}, "ok")],
            )
            good_c1_parsed = measure_cases.parse_transcript(good_c1)
            self.assertEqual(good_c1_parsed["wrapper_ragrep_search_count"], 1)
            self.assertEqual(measure_cases.audit_transcript(
                good_c1_parsed, "c1_ragrep_no_skill",
                frozen_wrapper_path=r"D:\run\wrapper.ps1",
            ), [])
            good_c2 = self.write_transcript(
                root / "good-c2.jsonl",
                calls=[
                    ("shell", "shell_command", {"command": r"Get-Content D:\fixed\skills\search\SKILL.md"}, "ok"),
                    ("shell", "shell_command", {"command": r"D:\run\wrapper.ps1 ragrep search x"}, "ok"),
                ],
            )
            self.assertEqual(measure_cases.audit_transcript(
                measure_cases.parse_transcript(good_c2), "c2_skill_ragrep",
                frozen_skill_path=r"D:\fixed\skills\search\SKILL.md",
                frozen_wrapper_path=r"D:\run\wrapper.ps1",
            ), [])

    def test_audit_requires_real_exact_wrapper_and_skill_operations(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            wrapper = r"D:\run\wrapper.ps1"
            skill = r"D:\fixed\skills\search\SKILL.md"
            cases = [
                ("c0_no_skill_no_ragrep", [("shell", "shell_command", {"command": "ragrep get --doc Manual/a.md"}, "ok")]),
                ("c1_ragrep_no_skill", [("shell", "shell_command", {"command": f"echo {wrapper}; ragrep search x"}, "ok")]),
                ("c2_skill_ragrep", [
                    ("shell", "shell_command", {"command": f"echo {skill}"}, "ok"),
                    ("shell", "shell_command", {"command": f"& '{wrapper}' search x"}, "ok"),
                ]),
            ]
            for number, (condition, calls) in enumerate(cases):
                path = self.write_transcript(root / f"structured-{number}.jsonl", calls=calls)
                violations = measure_cases.audit_transcript(
                    measure_cases.parse_transcript(path), condition,
                    frozen_skill_path=skill, frozen_wrapper_path=wrapper,
                )
                self.assertTrue(violations, (condition, calls))

            bypass = self.write_transcript(
                root / "bypass.jsonl",
                calls=[("shell", "shell_command", {"command": "ragrep search x"}, "ok")],
            )
            self.assertIn(
                "forbidden ragrep wrapper bypass",
                measure_cases.audit_transcript(
                    measure_cases.parse_transcript(bypass), "c1_ragrep_no_skill",
                    frozen_wrapper_path=wrapper,
                ),
            )

    def test_forbidden_tool_output_is_hashed_but_never_excerpted(self):
        with tempfile.TemporaryDirectory() as raw:
            secret = "holdout-gold.jsonl SECRET ANSWER"
            path = self.write_transcript(
                Path(raw) / "secret.jsonl",
                calls=[("shell", "shell_command", {"command": "Get-Content holdout-gold.jsonl"}, secret)],
            )

            parsed = measure_cases.parse_transcript(path)

            self.assertEqual(len(parsed["tool_calls"][0]["output_sha256"]), 64)
            self.assertIsNone(parsed["tool_calls"][0]["output_excerpt"])
            self.assertNotIn("SECRET ANSWER", json.dumps(parsed))


class ResultLedgerTests(unittest.TestCase):
    run_key = "c0_no_skill_no_ragrep/r1/q-1"

    def make_run(self, root: Path):
        root.mkdir()
        (root / "dispatch.jsonl").write_text(json.dumps({
            "run_key": self.run_key, "condition": "c0_no_skill_no_ragrep",
            "repetition": 1, "order": 1, "id": "q-1", "query": "q",
            "split": "development",
        }) + "\n", encoding="utf-8")
        source_paths = {}
        for name in (
            "frozen_manifest", "holdout_questions", "development", "holdout_gold",
            "validator", "skill", "db", "binary", "wrapper",
        ):
            path = root.parent / name
            path.write_text(name, encoding="utf-8")
            source_paths[name] = str(path.resolve())
        prompt_paths = {}
        for condition in measure_cases.CONDITIONS:
            path = root.parent / f"{condition}.prompt"
            path.write_text(condition, encoding="utf-8")
            prompt_paths[condition] = str(path.resolve())
        controller_inputs = root.parent / "controller-inputs.json"
        controller_inputs.write_text(json.dumps({
            "sources": source_paths, "prompts": prompt_paths,
        }), encoding="utf-8")
        (root / "manifest.json").write_text(json.dumps({
            "model": "gpt-5.6-sol", "reasoning": "medium", "timeout_seconds": 600,
            "prompt_hashes": {
                condition: measure_cases.sha256_file(Path(path))
                for condition, path in prompt_paths.items()
            },
            "sha256": {
                name: measure_cases.sha256_file(Path(path))
                for name, path in source_paths.items()
            },
            "skill_path": source_paths["skill"],
            "wrapper_path": source_paths["wrapper"],
            "controller_inputs_sha256": measure_cases.sha256_file(controller_inputs),
        }), encoding="utf-8")
        transcript = root.parent / "transcript.jsonl"
        TranscriptTests().write_transcript(transcript, calls=[])
        final = json.dumps({
            "id": "q-1", "answerable": True, "answer": "a",
            "evidence": [{"doc": "Manual/a.md", "para": 1}],
            "searches": [], "retrievals": [], "notes": "",
        })
        return transcript, final, controller_inputs

    def record(self, root, thread="thread-1", status="completed", transcript=None, final=None):
        controller_inputs = root.parent / "controller-inputs.json"
        if transcript is None or final is None:
            transcript, final, controller_inputs = self.make_run(root)
        claimed = measure_cases.claim_runs(
            root, controller_inputs, 1,
            datetime(2026, 8, 12, 0, tzinfo=timezone.utc),
        )
        self.assertEqual([row["run_key"] for row in claimed], [self.run_key])
        measure_cases.assign_claim(
            root, self.run_key, thread,
            datetime(2026, 8, 12, 0, 30, tzinfo=timezone.utc),
        )
        return measure_cases.record_result(
            root, self.run_key, thread, status, final, transcript,
            datetime(2026, 8, 12, 1, tzinfo=timezone.utc),
            controller_inputs=controller_inputs,
        )

    def test_record_result_appends_verified_hash_chain_and_rejects_duplicates(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            record = self.record(root)
            self.assertTrue(record["archive_ready"])
            results = [json.loads(line) for line in (root / "results.jsonl").read_text().splitlines()]
            chain = [json.loads(line) for line in (root / "results.sha256-chain.jsonl").read_text().splitlines()]
            self.assertEqual(len(results), 1)
            self.assertEqual(chain[0]["run_key"], self.run_key)
            self.assertEqual(chain[0]["previous_chain_sha256"], "0" * 64)
            self.assertEqual(len(chain[0]["record_sha256"]), 64)
            self.assertEqual(len(chain[0]["chain_sha256"]), 64)
            verified = measure_cases.verify_results(root)
            self.assertTrue(verified["valid"])
            self.assertEqual(verified["unarchived_thread_ids"], ["thread-1"])
            with self.assertRaisesRegex(ValueError, "duplicate"):
                measure_cases.record_result(
                    root, self.run_key, "thread-2", "completed", "{}",
                    root.parent / "transcript.jsonl", datetime.now(timezone.utc),
                    controller_inputs=root.parent / "controller-inputs.json",
                )
            with self.assertRaisesRegex(ValueError, "unknown"):
                measure_cases.record_result(
                    root, "unknown", "thread-2", "timeout", "",
                    root.parent / "transcript.jsonl", datetime.now(timezone.utc),
                    controller_inputs=root.parent / "controller-inputs.json",
                )

    def test_verify_recovers_only_missing_final_chain_row(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            self.record(root)
            (root / "results.sha256-chain.jsonl").write_text("", encoding="utf-8")
            self.assertTrue(measure_cases.verify_results(root)["valid"])
            self.assertEqual(len((root / "results.sha256-chain.jsonl").read_text().splitlines()), 1)
            (root / "results.sha256-chain.jsonl").write_text(
                json.dumps({"run_key": self.run_key, "record_sha256": "f" * 64,
                            "previous_chain_sha256": "0" * 64, "chain_sha256": "e" * 64}) + "\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ValueError, "chain"):
                measure_cases.verify_results(root)

    def test_archive_gate_and_infrastructure_redispatch(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            transcript, final, _ = self.make_run(root)
            infra = self.record(root, status="infrastructure_invalid", transcript=transcript, final=final)
            self.assertTrue(infra["archive_ready"])
            self.assertEqual([row["run_key"] for row in measure_cases.pending_runs(
                root, 8, controller_inputs=root.parent / "controller-inputs.json"
            )], [self.run_key])
            measure_cases.mark_archived(
                root, self.run_key, "thread-1", datetime(2026, 8, 12, 2, tzinfo=timezone.utc)
            )
            with self.assertRaisesRegex(ValueError, "duplicate"):
                measure_cases.mark_archived(
                    root, self.run_key, "thread-1", datetime.now(timezone.utc)
                )
            transcript2 = root.parent / "transcript-2.jsonl"
            TranscriptTests().write_transcript(transcript2, thread_id="thread-2", calls=[])
            completed = self.record(root, thread="thread-2", transcript=transcript2, final=final)
            self.assertTrue(completed["archive_ready"])
            self.assertEqual(measure_cases.pending_runs(
                root, 8, controller_inputs=root.parent / "controller-inputs.json"
            ), [])

    def test_missing_token_event_stays_null_and_status_is_closed_enum(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            transcript, final, _ = self.make_run(root)
            transcript.write_text(json.dumps({
                "timestamp": "2026-08-12T00:00:00Z", "type": "session_meta",
                "payload": {"id": "thread-1"},
            }) + "\n", encoding="utf-8")
            record = self.record(root, transcript=transcript, final=final)
            self.assertEqual(record["tokens"], {key: None for key in measure_cases.TOKEN_KEYS})
            with tempfile.TemporaryDirectory() as other:
                other_root = Path(other) / "run"
                other_transcript, other_final, _ = self.make_run(other_root)
                with self.assertRaisesRegex(ValueError, "status"):
                    self.record(other_root, status="retry", transcript=other_transcript, final=other_final)

    def test_result_cli_reads_worker_json_from_stdin(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            transcript, final, _ = self.make_run(root)
            measure_cases.claim_runs(
                root, root.parent / "controller-inputs.json", 1,
                datetime(2026, 8, 12, 0, tzinfo=timezone.utc),
            )
            measure_cases.assign_claim(
                root, self.run_key, "thread-1",
                datetime(2026, 8, 12, 0, 30, tzinfo=timezone.utc),
            )
            stdout = io.StringIO()
            with mock.patch("sys.stdin", io.StringIO(final)), contextlib.redirect_stdout(stdout):
                self.assertEqual(measure_cases.main([
                    "record", "--run-dir", str(root), "--run-key", self.run_key,
                    "--thread-id", "thread-1", "--status", "completed",
                    "--transcript", str(transcript), "--completed-at", "2026-08-12T01:00:00Z",
                    "--controller-inputs", str(root.parent / "controller-inputs.json"),
                ]), 0)
            self.assertTrue(json.loads(stdout.getvalue())["archive_ready"])
            stdout = io.StringIO()
            with contextlib.redirect_stdout(stdout):
                self.assertEqual(measure_cases.main([
                    "verify", "--run-dir", str(root),
                    "--controller-inputs", str(root.parent / "controller-inputs.json"),
                ]), 0)
            self.assertTrue(json.loads(stdout.getvalue())["valid"])
            stdout = io.StringIO()
            with contextlib.redirect_stdout(stdout):
                self.assertEqual(measure_cases.main([
                    "pending", "--run-dir", str(root), "--limit", "8",
                    "--controller-inputs", str(root.parent / "controller-inputs.json"),
                ]), 0)
            self.assertEqual(stdout.getvalue(), "")
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(measure_cases.main([
                    "mark-archived", "--run-dir", str(root), "--run-key", self.run_key,
                    "--thread-id", "thread-1", "--archived-at", "2026-08-12T02:00:00Z",
                ]), 0)

    def test_timeout_private_access_is_invalid_leakage(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            _, final, controller_inputs = self.make_run(root)
            transcript = root.parent / "leak.jsonl"
            TranscriptTests().write_transcript(
                transcript,
                calls=[("shell", "shell_command", {"command": "Get-Content holdout-gold.jsonl"}, "SECRET")],
            )
            measure_cases.claim_runs(
                root, controller_inputs, 1,
                datetime(2026, 8, 12, 0, tzinfo=timezone.utc),
            )
            measure_cases.assign_claim(
                root, self.run_key, "thread-1",
                datetime(2026, 8, 12, 0, 30, tzinfo=timezone.utc),
            )

            record = measure_cases.record_result(
                root, self.run_key, "thread-1", "timeout", final, transcript,
                datetime(2026, 8, 12, 1, tzinfo=timezone.utc),
                controller_inputs=controller_inputs,
            )

            self.assertEqual(record["status"], "invalid_leakage")
            self.assertNotIn("SECRET", json.dumps(record))

    def test_infrastructure_failure_without_forbidden_access_remains_retryable(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            _, final, controller_inputs = self.make_run(root)
            dispatch = json.loads((root / "dispatch.jsonl").read_text())
            dispatch["condition"] = "c1_ragrep_no_skill"
            dispatch["run_key"] = "c1_ragrep_no_skill/r1/q-1"
            (root / "dispatch.jsonl").write_text(json.dumps(dispatch) + "\n", encoding="utf-8")
            transcript = root.parent / "infra.jsonl"
            TranscriptTests().write_transcript(transcript, calls=[])
            measure_cases.claim_runs(root, controller_inputs, 1, datetime(2026, 8, 12, tzinfo=timezone.utc))
            measure_cases.assign_claim(
                root, dispatch["run_key"], "thread-1",
                datetime(2026, 8, 12, 0, 30, tzinfo=timezone.utc),
            )

            record = measure_cases.record_result(
                root, dispatch["run_key"], "thread-1", "infrastructure_invalid",
                final, transcript, datetime(2026, 8, 12, 1, tzinfo=timezone.utc),
                controller_inputs=controller_inputs,
            )

            self.assertEqual(record["status"], "infrastructure_invalid")

    def test_claim_is_durable_assigned_and_recoverable(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            _, _, controller_inputs = self.make_run(root)
            claimed = measure_cases.claim_runs(
                root, controller_inputs, 1,
                datetime(2026, 8, 12, 0, tzinfo=timezone.utc),
            )
            self.assertEqual([row["run_key"] for row in claimed], [self.run_key])
            self.assertEqual(measure_cases.claim_runs(
                root, controller_inputs, 1,
                datetime(2026, 8, 12, 0, 1, tzinfo=timezone.utc),
            ), [])
            measure_cases.assign_claim(
                root, self.run_key, "thread-1",
                datetime(2026, 8, 12, 0, 2, tzinfo=timezone.utc),
            )
            with self.assertRaisesRegex(ValueError, "assigned"):
                measure_cases.recover_claim(
                    root, self.run_key,
                    datetime(2026, 8, 12, 0, 3, tzinfo=timezone.utc),
                )

    def test_resume_rehashes_every_fixed_input_before_pending_or_record(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            transcript, final, controller_inputs = self.make_run(root)
            self.assertTrue(measure_cases.verify_inputs(root, controller_inputs)["valid"])
            (root.parent / "db").write_text("drift", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "db"):
                measure_cases.pending_runs(root, 8, controller_inputs=controller_inputs)
            with self.assertRaisesRegex(ValueError, "db"):
                measure_cases.record_result(
                    root, self.run_key, "thread-1", "completed", final, transcript,
                    datetime(2026, 8, 12, 1, tzinfo=timezone.utc),
                    controller_inputs=controller_inputs,
                )

    def test_verify_rejects_shared_thread_and_mismatched_archive_pairs(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw) / "run"
            self.record(root)
            dispatch = json.loads((root / "dispatch.jsonl").read_text())
            second_dispatch = dict(dispatch, run_key="c0_no_skill_no_ragrep/r1/q-2", id="q-2", order=2)
            with (root / "dispatch.jsonl").open("a", encoding="utf-8") as destination:
                destination.write(json.dumps(second_dispatch) + "\n")
            first = json.loads((root / "results.jsonl").read_text())
            second = dict(first, run_key=second_dispatch["run_key"], id="q-2", order=2)
            second["worker_output"] = dict(first["worker_output"], id="q-2")
            (root / "results.jsonl").write_text(
                json.dumps(first) + "\n" + json.dumps(second) + "\n", encoding="utf-8",
            )
            first_chain = measure_cases._chain_row(first, measure_cases.GENESIS_SHA256)
            second_chain = measure_cases._chain_row(second, first_chain["chain_sha256"])
            (root / "results.sha256-chain.jsonl").write_text(
                json.dumps(first_chain) + "\n" + json.dumps(second_chain) + "\n", encoding="utf-8",
            )
            (root / "archive.jsonl").write_text(json.dumps({
                "run_key": self.run_key, "thread_id": "thread-1",
                "archived_at": "2026-08-12T02:00:00Z",
            }) + "\n", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "thread"):
                measure_cases.verify_results(root)


if __name__ == "__main__":
    unittest.main()
