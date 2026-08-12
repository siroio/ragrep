import argparse
from datetime import datetime, timezone
from collections import Counter
import hashlib
import json
import os
from pathlib import Path, PurePosixPath, PureWindowsPath
import random
import shutil
import sys
import tempfile

from validate_cases import load_jsonl


CONDITIONS = (
    "c0_no_skill_no_ragrep",
    "c1_ragrep_no_skill",
    "c2_skill_ragrep",
)
SEED_ALGORITHM = "sha256(seed:condition:repetition)"
WORKER_KEYS = {
    "id", "answerable", "answer", "evidence", "searches", "retrievals", "notes"
}
TOKEN_KEYS = (
    "input_tokens", "cached_input_tokens", "output_tokens",
    "reasoning_output_tokens", "total_tokens",
)
FORBIDDEN_MARKERS = (
    "ragrep-eval-private", "holdout-gold.jsonl", "review.jsonl",
    "allocation.json", "results.jsonl", "grades-a.jsonl", "grades-b.jsonl",
)
RESULT_STATUSES = {
    "completed", "timeout", "task_error", "needs_input", "invalid_output",
    "invalid_leakage", "infrastructure_invalid",
}
GENESIS_SHA256 = "0" * 64


def load_questions(development: Path, holdout_questions: Path) -> list[dict]:
    questions = []
    seen = set()
    for split, path in (
        ("development", development),
        ("holdout", holdout_questions),
    ):
        for source in load_jsonl(path):
            if split == "holdout" and set(source) != {"id", "query"}:
                raise ValueError("holdout questions must contain exactly id and query")
            identifier = source.get("id")
            query = source.get("query")
            if not isinstance(identifier, str) or not identifier.strip():
                raise ValueError("question requires a non-empty string id")
            if not isinstance(query, str) or not query.strip():
                raise ValueError(f"{identifier}: question requires a non-empty string query")
            if identifier in seen:
                raise ValueError(f"{identifier}: duplicate question id")
            seen.add(identifier)
            questions.append({"id": identifier, "query": query, "split": split})
    return questions


def build_dispatch(
    questions: list[dict], repetitions: int, seed: int
) -> list[dict]:
    if repetitions < 1:
        raise ValueError("repetitions must be positive")
    dispatch = []
    for condition in CONDITIONS:
        for repetition in range(1, repetitions + 1):
            material = f"{seed}:{condition}:{repetition}".encode()
            local_seed = int.from_bytes(hashlib.sha256(material).digest())
            shuffled = list(questions)
            random.Random(local_seed).shuffle(shuffled)
            for order, question in enumerate(shuffled, start=1):
                dispatch.append({
                    "condition": condition,
                    "repetition": repetition,
                    "order": order,
                    "id": question["id"],
                    "query": question["query"],
                    "split": question["split"],
                    "run_key": f'{condition}/r{repetition}/{question["id"]}',
                })
    return dispatch


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _relative_doc(value: object) -> bool:
    if not isinstance(value, str) or not value.strip():
        return False
    posix = PurePosixPath(value)
    windows = PureWindowsPath(value)
    return not posix.is_absolute() and not windows.is_absolute() and ".." not in posix.parts


def _validate_doc_para(value: object, *, rank: bool = False) -> None:
    keys = {"rank", "doc", "para"} if rank else {"doc", "para"}
    if not isinstance(value, dict) or set(value) != keys:
        raise ValueError(f"entry must contain exactly {sorted(keys)}")
    if not _relative_doc(value["doc"]):
        raise ValueError("document path must be relative and non-escaping")
    if not isinstance(value["para"], int) or isinstance(value["para"], bool) or value["para"] < 0:
        raise ValueError("para must be a non-negative integer")
    if rank and (
        not isinstance(value["rank"], int)
        or isinstance(value["rank"], bool)
        or value["rank"] < 1
    ):
        raise ValueError("rank must be a positive integer")


def parse_worker_output(text: str, expected_id: str, condition: str) -> dict:
    value = json.loads(text)
    if not isinstance(value, dict) or set(value) != WORKER_KEYS:
        raise ValueError("worker output has invalid fields")
    if value["id"] != expected_id:
        raise ValueError("worker output id mismatch")
    if type(value["answerable"]) is not bool:
        raise ValueError("answerable must be a boolean")
    if not isinstance(value["answer"], str) or not isinstance(value["notes"], str):
        raise ValueError("answer and notes must be strings")
    if not isinstance(value["evidence"], list):
        raise ValueError("evidence must be an array")
    for evidence in value["evidence"]:
        _validate_doc_para(evidence)
    if value["answerable"] and not value["evidence"] and not value["notes"].strip():
        raise ValueError("answerable output requires evidence or an explicit error note")
    if not isinstance(value["searches"], list) or any(
        not isinstance(search, str) or not search.strip() for search in value["searches"]
    ):
        raise ValueError("searches must contain non-empty strings")
    if not isinstance(value["retrievals"], list):
        raise ValueError("retrievals must be an array")
    for retrieval in value["retrievals"]:
        if not isinstance(retrieval, dict) or set(retrieval) != {"query", "mode", "hits"}:
            raise ValueError("retrieval has invalid fields")
        if not isinstance(retrieval["query"], str) or not retrieval["query"].strip():
            raise ValueError("retrieval query must be non-empty")
        if retrieval["mode"] not in {"text", "hybrid", "vector"}:
            raise ValueError("retrieval mode is invalid")
        if not isinstance(retrieval["hits"], list):
            raise ValueError("retrieval hits must be an array")
        ranks = []
        for hit in retrieval["hits"]:
            _validate_doc_para(hit, rank=True)
            ranks.append(hit["rank"])
        if len(ranks) != len(set(ranks)):
            raise ValueError("retrieval hit ranks must be unique")
    if condition not in CONDITIONS:
        raise ValueError("unknown condition")
    if condition == CONDITIONS[0] and value["retrievals"]:
        raise ValueError("C0 must not contain retrievals")
    if condition != CONDITIONS[0] and (not value["searches"] or not value["retrievals"]):
        raise ValueError("ragrep conditions require searches and retrievals")
    return value


def _jsonl_objects(path: Path):
    for line in path.read_text(encoding="utf-8").splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            yield value


def _session_id(path: Path) -> str | None:
    for row in _jsonl_objects(path):
        if row.get("type") == "session_meta" and isinstance(row.get("payload"), dict):
            identifier = row["payload"].get("id")
            return identifier if isinstance(identifier, str) else None
    return None


def find_transcript(sessions_root: Path, thread_id: str) -> Path:
    matches = [
        path for path in sessions_root.rglob("*.jsonl")
        if _session_id(path) == thread_id
    ]
    if not matches:
        raise ValueError(f"no transcript for {thread_id}")
    if len(matches) != 1:
        raise ValueError(f"multiple transcripts for {thread_id}")
    return matches[0]


def _text(value: object) -> str:
    return value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, sort_keys=True)


def _audit_text(value: object) -> str:
    return _text(value).lower().replace("/", "\\").replace("\\\\", "\\")


def _timestamp(value: object) -> datetime | None:
    if not isinstance(value, str):
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def parse_transcript(path: Path) -> dict:
    calls = {}
    outputs = {}
    tool_counts = Counter()
    tokens = None
    thread_id = None
    timestamps = []
    for row in _jsonl_objects(path):
        when = _timestamp(row.get("timestamp"))
        if when is not None:
            timestamps.append(when)
        payload = row.get("payload")
        if not isinstance(payload, dict):
            continue
        if row.get("type") == "session_meta" and isinstance(payload.get("id"), str):
            thread_id = payload["id"]
        if (
            row.get("type") == "event_msg"
            and payload.get("type") == "token_count"
            and isinstance(payload.get("info"), dict)
            and isinstance(payload["info"].get("last_token_usage"), dict)
        ):
            usage = payload["info"]["last_token_usage"]
            tokens = {key: usage.get(key) for key in TOKEN_KEYS}
        if row.get("type") != "response_item":
            continue
        kind = payload.get("type")
        call_id = payload.get("call_id")
        if kind in {"function_call", "custom_tool_call"} and isinstance(call_id, str):
            namespace = payload.get("namespace")
            name = payload.get("name")
            if not isinstance(name, str):
                continue
            tool_name = f"{namespace}__{name}" if isinstance(namespace, str) else name
            raw_arguments = payload.get("arguments", payload.get("input", {}))
            if isinstance(raw_arguments, str):
                try:
                    arguments = json.loads(raw_arguments)
                except json.JSONDecodeError:
                    arguments = raw_arguments
            else:
                arguments = raw_arguments
            calls[call_id] = {"name": tool_name, "arguments": arguments}
            tool_counts[tool_name] += 1
        elif kind in {"function_call_output", "custom_tool_call_output"} and isinstance(call_id, str):
            outputs[call_id] = payload.get("output", "")
    traces = []
    for call_id, call in calls.items():
        output_text = _text(outputs.get(call_id, ""))
        lowered_output = _audit_text(output_text)
        traces.append({
            **call,
            "output_sha256": hashlib.sha256(output_text.encode()).hexdigest(),
            "output_excerpt": output_text[:512],
            "output_markers": [marker for marker in FORBIDDEN_MARKERS if marker in lowered_output],
            "output_mentions_skill": "skills\\search\\skill.md" in lowered_output,
            "output_mentions_ragrep_search": "ragrep" in lowered_output and "search" in lowered_output,
        })
    argument_texts = [_text(trace["arguments"]).lower() for trace in traces]
    all_arguments = "\n".join(argument_texts)
    return {
        "thread_id": thread_id,
        "tokens": tokens or {key: None for key in TOKEN_KEYS},
        "tool_counts": dict(sorted(tool_counts.items())),
        "tool_calls": traces,
        "ragrep_search_count": sum(
            1 for value in argument_texts if "ragrep" in value and "search" in value
        ),
        "wrapper_ragrep_search_count": sum(
            1 for value in argument_texts
            if "ragrep" in value and "search" in value
            and ("wrapper" in value or ".ps1" in value)
        ),
        "rg_count": sum(1 for value in argument_texts if any(
            part == "rg" or part.endswith("\\rg") for part in value.replace('"', " ").split()
        )),
        "file_read_count": sum(
            1 for trace in traces
            if "read" in trace["name"].lower()
            or "get-content" in _text(trace["arguments"]).lower()
        ),
        "started_at": min(timestamps).isoformat().replace("+00:00", "Z") if timestamps else None,
        "completed_at": max(timestamps).isoformat().replace("+00:00", "Z") if timestamps else None,
        "duration_seconds": (max(timestamps) - min(timestamps)).total_seconds() if timestamps else None,
        "source_transcript": str(path.resolve()),
        "transcript_sha256": sha256_file(path),
    }


def audit_transcript(
    parsed: dict, condition: str, frozen_skill_path: str | None = None
) -> list[str]:
    violations = []
    calls = parsed["tool_calls"]
    for call in calls:
        arguments = _audit_text(call["arguments"])
        for marker in FORBIDDEN_MARKERS:
            if marker in arguments or marker in call["output_markers"]:
                violations.append(f"forbidden access: {marker}")
        mentions_skill = "skills\\search\\skill.md" in arguments or call["output_mentions_skill"]
        if condition in CONDITIONS[:2] and mentions_skill:
            violations.append("forbidden skill access")
        name = call["name"].lower()
        if condition == CONDITIONS[0] and (
            ("ragrep" in arguments and "search" in arguments)
            or call["output_mentions_ragrep_search"]
            or (name.startswith("mcp__") and "search" in name)
        ):
            violations.append("forbidden search tool")
    if condition in CONDITIONS[1:]:
        if parsed["ragrep_search_count"] < 1:
            violations.append("required ragrep search missing")
        elif parsed["wrapper_ragrep_search_count"] < 1:
            violations.append("ragrep search did not use a wrapper")
    if condition == CONDITIONS[2]:
        if not frozen_skill_path:
            violations.append("frozen skill path missing")
        else:
            wanted = _audit_text(frozen_skill_path)
            if not any(wanted in _audit_text(call["arguments"]) for call in calls):
                violations.append("required frozen skill read missing")
    return sorted(set(violations))


def _ledger_rows(path: Path) -> list[dict]:
    return load_jsonl(path) if path.exists() else []


def _canonical(value: dict) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode()


def _append_jsonl(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8", newline="\n") as destination:
        destination.write(_canonical(value).decode() + "\n")
        destination.flush()
        os.fsync(destination.fileno())


def _chain_row(record: dict, previous: str) -> dict:
    record_sha = hashlib.sha256(_canonical(record)).hexdigest()
    chain_sha = hashlib.sha256((previous + record_sha).encode()).hexdigest()
    return {
        "run_key": record["run_key"],
        "record_sha256": record_sha,
        "previous_chain_sha256": previous,
        "chain_sha256": chain_sha,
    }


def verify_results(run_dir: Path) -> dict:
    dispatch = _ledger_rows(run_dir / "dispatch.jsonl")
    dispatch_keys = [row.get("run_key") for row in dispatch]
    if len(dispatch_keys) != len(set(dispatch_keys)) or any(not isinstance(key, str) for key in dispatch_keys):
        raise ValueError("dispatch contains invalid or duplicate run keys")
    expected = set(dispatch_keys)
    results = _ledger_rows(run_dir / "results.jsonl")
    chain = _ledger_rows(run_dir / "results.sha256-chain.jsonl")
    if len(chain) > len(results) or len(results) - len(chain) > 1:
        raise ValueError("results chain length mismatch")
    previous = GENESIS_SHA256
    for index, record in enumerate(results):
        if record.get("run_key") not in expected:
            raise ValueError(f'unknown run key: {record.get("run_key")}')
        if record.get("status") not in RESULT_STATUSES:
            raise ValueError("invalid result status")
        wanted = _chain_row(record, previous)
        if index < len(chain):
            if chain[index] != wanted:
                raise ValueError(f"chain mismatch at row {index + 1}")
        else:
            _append_jsonl(run_dir / "results.sha256-chain.jsonl", wanted)
            chain.append(wanted)
        previous = wanted["chain_sha256"]
    valid_rows = [row for row in results if row["status"] != "infrastructure_invalid"]
    counts = Counter(row["run_key"] for row in valid_rows)
    duplicates = sorted(key for key, count in counts.items() if count > 1)
    if duplicates:
        raise ValueError(f"duplicate result: {duplicates[0]}")
    archives = _ledger_rows(run_dir / "archive.jsonl")
    archived_threads = {row.get("thread_id") for row in archives}
    return {
        "valid": True,
        "result_count": len(valid_rows),
        "infrastructure_invalid_count": len(results) - len(valid_rows),
        "missing_run_keys": sorted(expected - set(counts)),
        "duplicate_run_keys": duplicates,
        "unarchived_thread_ids": [
            row["thread_id"] for row in results if row.get("thread_id") not in archived_threads
        ],
        "archive_ready": len(chain) == len(results),
    }


def record_result(
    run_dir: Path,
    run_key: str,
    thread_id: str,
    status: str,
    final_text: str,
    transcript: Path,
    completed_at: datetime,
) -> dict:
    if status not in RESULT_STATUSES:
        raise ValueError(f"invalid status: {status}")
    if completed_at.tzinfo is None or completed_at.utcoffset() is None:
        raise ValueError("completed_at requires a timezone")
    dispatch = {row["run_key"]: row for row in _ledger_rows(run_dir / "dispatch.jsonl")}
    if run_key not in dispatch:
        raise ValueError(f"unknown run key: {run_key}")
    existing = _ledger_rows(run_dir / "results.jsonl")
    if any(
        row["run_key"] == run_key and row["status"] != "infrastructure_invalid"
        for row in existing
    ):
        raise ValueError(f"duplicate result: {run_key}")
    verify_results(run_dir)
    trace = parse_transcript(transcript)
    if trace["thread_id"] != thread_id:
        raise ValueError("transcript thread id mismatch")
    item = dispatch[run_key]
    parsed_output = None
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    violations = audit_transcript(
        trace, item["condition"], frozen_skill_path=manifest.get("skill_path")
    )
    effective_status = status
    if status == "completed":
        try:
            parsed_output = parse_worker_output(final_text, item["id"], item["condition"])
        except (ValueError, json.JSONDecodeError):
            effective_status = "invalid_output"
        if violations:
            effective_status = "invalid_leakage"
    record = {
        key: item[key] for key in (
            "run_key", "condition", "repetition", "order", "id", "split"
        )
    }
    record.update({
        "thread_id": thread_id,
        "model": manifest.get("model"),
        "reasoning": manifest.get("reasoning"),
        "timeout_seconds": manifest.get("timeout_seconds"),
        "completed_at": completed_at.astimezone(timezone.utc).isoformat().replace("+00:00", "Z"),
        "duration_seconds": trace["duration_seconds"],
        "status": effective_status,
        "worker_output": parsed_output,
        "violations": violations,
        "tokens": trace["tokens"],
        "tool_counts": trace["tool_counts"],
        "ragrep_search_count": trace["ragrep_search_count"],
        "wrapper_ragrep_search_count": trace["wrapper_ragrep_search_count"],
        "rg_count": trace["rg_count"],
        "file_read_count": trace["file_read_count"],
        "tool_calls": trace["tool_calls"],
        "source_transcript": trace["source_transcript"],
        "transcript_sha256": trace["transcript_sha256"],
        "prompt_sha256": manifest.get("prompt_hashes", {}).get(item["condition"]),
        "binary_sha256": manifest.get("sha256", {}).get("binary"),
        "db_sha256": manifest.get("sha256", {}).get("db"),
        "skill_sha256": None if item["condition"] == CONDITIONS[0] else manifest.get("sha256", {}).get("skill"),
    })
    _append_jsonl(run_dir / "results.jsonl", record)
    previous = GENESIS_SHA256
    chain = _ledger_rows(run_dir / "results.sha256-chain.jsonl")
    if chain:
        previous = chain[-1]["chain_sha256"]
    _append_jsonl(run_dir / "results.sha256-chain.jsonl", _chain_row(record, previous))
    verified = verify_results(run_dir)
    return {**record, "archive_ready": verified["archive_ready"]}


def pending_runs(run_dir: Path, limit: int = 8) -> list[dict]:
    if limit < 1:
        raise ValueError("limit must be positive")
    dispatch = _ledger_rows(run_dir / "dispatch.jsonl")
    completed = {
        row["run_key"] for row in _ledger_rows(run_dir / "results.jsonl")
        if row["status"] != "infrastructure_invalid"
    }
    active = {
        row.get("run_key") for row in _ledger_rows(run_dir / "active.jsonl")
        if row.get("status", "active") == "active"
    }
    return [row for row in dispatch if row["run_key"] not in completed | active][:limit]


def mark_archived(
    run_dir: Path, run_key: str, thread_id: str, archived_at: datetime
) -> dict:
    if archived_at.tzinfo is None or archived_at.utcoffset() is None:
        raise ValueError("archived_at requires a timezone")
    verified = verify_results(run_dir)
    if not verified["archive_ready"]:
        raise ValueError("result is not archive ready")
    results = _ledger_rows(run_dir / "results.jsonl")
    if not any(row["run_key"] == run_key and row["thread_id"] == thread_id for row in results):
        raise ValueError("archive event does not match a result")
    archives = _ledger_rows(run_dir / "archive.jsonl")
    if any(row.get("thread_id") == thread_id for row in archives):
        raise ValueError(f"duplicate archive event: {thread_id}")
    event = {
        "run_key": run_key,
        "thread_id": thread_id,
        "archived_at": archived_at.astimezone(timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    _append_jsonl(run_dir / "archive.jsonl", event)
    return event


def _write_jsonl(path: Path, rows: list[dict]) -> None:
    path.write_text(
        "".join(
            json.dumps(row, ensure_ascii=False, separators=(",", ":")) + "\n"
            for row in rows
        ),
        encoding="utf-8",
    )


def prepare_run(
    *,
    development: Path,
    holdout_questions: Path,
    frozen_manifest: Path,
    holdout_gold: Path,
    validator: Path,
    skill: Path,
    db: Path,
    binary: Path,
    output: Path,
    pilot_ids: tuple[str, str, str],
    seed: int,
    generated_at: datetime,
    prompt_hashes: dict[str, str],
    repetitions: int = 5,
) -> dict:
    if output.exists():
        raise ValueError(f"output already exists: {output}")
    if generated_at.tzinfo is None or generated_at.utcoffset() is None:
        raise ValueError("generated_at requires a timezone")
    if set(prompt_hashes) != set(CONDITIONS) or any(
        not isinstance(value, str) or len(value) != 64 for value in prompt_hashes.values()
    ):
        raise ValueError("prompt_hashes must contain one SHA-256 per condition")
    sources = {
        "frozen_manifest": frozen_manifest,
        "holdout_questions": holdout_questions,
        "development": development,
        "holdout_gold": holdout_gold,
        "validator": validator,
        "skill": skill,
        "db": db,
        "binary": binary,
    }
    fixed_hashes = {name: sha256_file(path) for name, path in sources.items()}
    questions = load_questions(development, holdout_questions)
    by_id = {question["id"]: question for question in questions}
    if len(set(pilot_ids)) != 3 or any(identifier not in by_id for identifier in pilot_ids):
        raise ValueError("pilot_ids must name three distinct questions")
    if any(by_id[identifier]["split"] != "development" for identifier in pilot_ids):
        raise ValueError("pilot questions must be Development questions")
    public_questions = []
    for question in questions:
        encoded = json.dumps(
            question, ensure_ascii=False, sort_keys=True, separators=(",", ":")
        ).encode()
        public_questions.append({**question, "question_sha256": hashlib.sha256(encoded).hexdigest()})
    dispatch = build_dispatch(questions, repetitions, seed)
    pilot = [
        {
            "condition": condition,
            "repetition": 1,
            "order": order,
            **by_id[identifier],
            "run_key": f"pilot/{condition}/{identifier}",
        }
        for condition in CONDITIONS
        for order, identifier in enumerate(pilot_ids, start=1)
    ]
    manifest = {
        "schema_version": 1,
        "generated_at": generated_at.astimezone(timezone.utc).isoformat().replace("+00:00", "Z"),
        "model": "gpt-5.6-sol",
        "reasoning": "medium",
        "timeout_seconds": 600,
        "concurrency": 8,
        "repetitions": repetitions,
        "seed": seed,
        "seed_algorithm": SEED_ALGORITHM,
        "question_count": len(questions),
        "run_count": len(dispatch),
        "pilot_run_count": len(pilot),
        "pilot_ids": list(pilot_ids),
        "prompt_hashes": dict(prompt_hashes),
        "sha256": fixed_hashes,
        "skill_path": str(skill.resolve()),
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    try:
        _write_jsonl(stage / "questions-only.jsonl", public_questions)
        _write_jsonl(stage / "dispatch.jsonl", dispatch)
        _write_jsonl(stage / "pilot-dispatch.jsonl", pilot)
        (stage / "manifest.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        stage.rename(output)
        return manifest
    finally:
        if stage.exists():
            shutil.rmtree(stage)


def _parse_datetime(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise ValueError("--generated-at requires a timezone")
    return parsed


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    prepare = subparsers.add_parser("prepare")
    for name in (
        "development", "holdout-questions", "frozen-manifest", "holdout-gold",
        "validator", "skill", "db", "binary", "output",
    ):
        prepare.add_argument(f"--{name}", type=Path, required=True)
    prepare.add_argument("--pilot-id", action="append", required=True)
    prepare.add_argument("--seed", type=int, required=True)
    prepare.add_argument("--generated-at", required=True)
    for condition in CONDITIONS:
        prepare.add_argument(f"--prompt-hash-{condition}", required=True)
    record = subparsers.add_parser("record")
    record.add_argument("--run-dir", type=Path, required=True)
    record.add_argument("--run-key", required=True)
    record.add_argument("--thread-id", required=True)
    record.add_argument("--status", choices=sorted(RESULT_STATUSES), required=True)
    record.add_argument("--transcript", type=Path, required=True)
    record.add_argument("--completed-at", required=True)
    verify = subparsers.add_parser("verify")
    verify.add_argument("--run-dir", type=Path, required=True)
    pending = subparsers.add_parser("pending")
    pending.add_argument("--run-dir", type=Path, required=True)
    pending.add_argument("--limit", type=int, default=8)
    archived = subparsers.add_parser("mark-archived")
    archived.add_argument("--run-dir", type=Path, required=True)
    archived.add_argument("--run-key", required=True)
    archived.add_argument("--thread-id", required=True)
    archived.add_argument("--archived-at", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "prepare":
            if len(args.pilot_id) != 3:
                raise ValueError("exactly three --pilot-id values are required")
            kwargs = vars(args)
            prompt_hashes = {
                condition: kwargs[f"prompt_hash_{condition}"] for condition in CONDITIONS
            }
            manifest = prepare_run(
                development=args.development,
                holdout_questions=args.holdout_questions,
                frozen_manifest=args.frozen_manifest,
                holdout_gold=args.holdout_gold,
                validator=args.validator,
                skill=args.skill,
                db=args.db,
                binary=args.binary,
                output=args.output,
                pilot_ids=tuple(args.pilot_id),
                seed=args.seed,
                generated_at=_parse_datetime(args.generated_at),
                prompt_hashes=prompt_hashes,
            )
            print(f'questions={manifest["question_count"]} runs={manifest["run_count"]} output={args.output}')
            return 0
        if args.command == "record":
            result = record_result(
                args.run_dir, args.run_key, args.thread_id, args.status,
                sys.stdin.read(), args.transcript, _parse_datetime(args.completed_at),
            )
            print(_canonical(result).decode())
            return 0
        if args.command == "verify":
            print(_canonical(verify_results(args.run_dir)).decode())
            return 0
        if args.command == "pending":
            for row in pending_runs(args.run_dir, args.limit):
                print(_canonical(row).decode())
            return 0
        if args.command == "mark-archived":
            event = mark_archived(
                args.run_dir, args.run_key, args.thread_id,
                _parse_datetime(args.archived_at),
            )
            print(_canonical(event).decode())
            return 0
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
