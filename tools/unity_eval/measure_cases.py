import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
from collections import Counter
import hashlib
import json
import os
from pathlib import Path, PurePosixPath, PureWindowsPath
import random
import re
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
RESULT_KEYS = {
    "run_key", "condition", "repetition", "order", "id", "split", "thread_id",
    "model", "reasoning", "timeout_seconds", "completed_at", "duration_seconds",
    "status", "worker_output", "violations", "tokens", "tool_counts",
    "ragrep_search_count", "wrapper_ragrep_search_count", "rg_count",
    "file_read_count", "tool_calls", "source_transcript", "transcript_sha256",
    "prompt_sha256", "binary_sha256", "db_sha256", "skill_sha256",
}


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
    queues = {}
    for repetition in range(1, repetitions + 1):
        for condition in CONDITIONS:
            material = f"{seed}:{condition}:{repetition}".encode()
            local_seed = int.from_bytes(hashlib.sha256(material).digest())
            shuffled = list(questions)
            random.Random(local_seed).shuffle(shuffled)
            queues[(condition, repetition)] = [
                {
                    "condition": condition,
                    "repetition": repetition,
                    "order": order,
                    "id": question["id"],
                    "query": question["query"],
                    "split": question["split"],
                    "run_key": f'{condition}/r{repetition}/{question["id"]}',
                }
                for order, question in enumerate(shuffled, start=1)
            ]
    dispatch = []
    for index in range(len(questions)):
        for repetition in range(1, repetitions + 1):
            for condition in CONDITIONS:
                dispatch.append(queues[(condition, repetition)][index])
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
    if "\\" in value:
        return False
    posix = PurePosixPath(value)
    windows = PureWindowsPath(value)
    return (
        not posix.is_absolute()
        and not windows.is_absolute()
        and not windows.drive
        and not windows.root
        and ".." not in posix.parts
        and ".." not in windows.parts
    )


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
        output_markers = [marker for marker in FORBIDDEN_MARKERS if marker in lowered_output]
        argument_markers = [
            marker for marker in FORBIDDEN_MARKERS
            if marker in _audit_text(call["arguments"])
        ]
        traces.append({
            **call,
            "output_sha256": hashlib.sha256(output_text.encode()).hexdigest(),
            "output_excerpt": None if output_markers or argument_markers else output_text[:512],
            "output_markers": output_markers,
            "output_mentions_skill": "skills\\search\\skill.md" in lowered_output,
            "output_mentions_ragrep": "ragrep" in lowered_output,
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


def _shell_command(call: dict) -> str | None:
    if "shell_command" not in call["name"].lower():
        return None
    arguments = call.get("arguments")
    if not isinstance(arguments, dict) or not isinstance(arguments.get("command"), str):
        return None
    return arguments["command"]


def _invokes_path(command: str, path: str, operation: str) -> bool:
    normalized = command.replace("/", "\\")
    wanted = path.replace("/", "\\")
    pattern = re.compile(
        rf"(?:^|[;|]\s*)\&?\s*['\"]?{re.escape(wanted)}['\"]?\s+"
        rf"(?:ragrep(?:\.exe)?\s+)?{re.escape(operation)}(?:\s|$)",
        re.IGNORECASE,
    )
    return bool(pattern.search(normalized))


def _reads_exact_path(call: dict, path: str) -> bool:
    wanted = _audit_text(path)
    command = _shell_command(call)
    if command is not None:
        normalized = command.replace("/", "\\")
        pattern = re.compile(
            rf"(?:^|[;|]\s*)(?:get-content|gc|type|more|cat)\s+['\"]?"
            rf"{re.escape(wanted)}['\"]?(?:\s|$)|"
            rf"(?:^|[;|]\s*)(?:rg|select-string)\b[^;|]*['\"]?"
            rf"{re.escape(wanted)}['\"]?(?:\s|$)",
            re.IGNORECASE,
        )
        if pattern.search(normalized.lower()):
            return True
    return "read" in call["name"].lower() and wanted in _audit_text(call["arguments"])


def _invokes_ragrep(call: dict, wrapper_path: str | None) -> bool:
    name = call["name"].lower()
    if (
        name.startswith("mcp__")
        and any(scope in name for scope in ("doc", "ragrep"))
        and any(operation in name for operation in ("search", "get"))
    ):
        return True
    command = _shell_command(call)
    if command is None:
        return False
    normalized = command.replace("/", "\\")
    if re.search(r"(?:^|[;|]\s*)\&?\s*['\"]?[^;|\s'\"]*ragrep(?:\.exe)?['\"]?(?:\s|$)", normalized, re.IGNORECASE):
        return True
    if not wrapper_path:
        return False
    wanted = wrapper_path.replace("/", "\\")
    return bool(re.search(
        rf"(?:^|[;|]\s*)\&?\s*['\"]?{re.escape(wanted)}['\"]?(?:\s|$)",
        normalized,
        re.IGNORECASE,
    ))


def audit_transcript(
    parsed: dict,
    condition: str,
    frozen_skill_path: str | None = None,
    frozen_wrapper_path: str | None = None,
) -> list[str]:
    violations = []
    calls = parsed["tool_calls"]
    for call in calls:
        arguments = _audit_text(call["arguments"])
        for marker in FORBIDDEN_MARKERS:
            if marker in arguments or marker in call["output_markers"]:
                violations.append(f"forbidden access: {marker}")
        reads_skill = bool(frozen_skill_path and _reads_exact_path(call, frozen_skill_path))
        if condition in CONDITIONS[:2] and reads_skill:
            violations.append("forbidden skill access")
        if condition == CONDITIONS[0] and (
            _invokes_ragrep(call, frozen_wrapper_path)
        ):
            violations.append("forbidden ragrep tool")
    if condition in CONDITIONS[1:]:
        exact_wrapper_search = bool(frozen_wrapper_path) and any(
            (command := _shell_command(call)) is not None
            and _invokes_path(command, frozen_wrapper_path, "search")
            for call in calls
        )
        raw_or_wrapper_ragrep = any(
            _invokes_ragrep(call, frozen_wrapper_path) for call in calls
        )
        if not frozen_wrapper_path:
            violations.append("frozen wrapper path missing")
        elif raw_or_wrapper_ragrep and not exact_wrapper_search:
            violations.append("forbidden ragrep wrapper bypass")
        elif not exact_wrapper_search:
            violations.append("required exact wrapper search missing")
    if condition == CONDITIONS[2]:
        if not frozen_skill_path:
            violations.append("frozen skill path missing")
        else:
            if not any(_reads_exact_path(call, frozen_skill_path) for call in calls):
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


def _write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("x", encoding="utf-8", newline="\n") as destination:
        destination.write(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
        destination.flush()
        os.fsync(destination.fileno())


def verify_inputs(run_dir: Path, controller_inputs: Path) -> dict:
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    if sha256_file(controller_inputs) != manifest.get("controller_inputs_sha256"):
        raise ValueError("controller inputs hash mismatch")
    identities = json.loads(controller_inputs.read_text(encoding="utf-8"))
    if set(identities) != {"sources", "prompts"}:
        raise ValueError("controller inputs have invalid fields")
    sources = identities["sources"]
    prompts = identities["prompts"]
    if set(sources) != set(manifest.get("sha256", {})):
        raise ValueError("fixed source identities mismatch")
    if set(prompts) != set(CONDITIONS):
        raise ValueError("prompt source identities mismatch")
    for name, raw_path in sources.items():
        path = Path(raw_path)
        if not path.is_absolute() or sha256_file(path) != manifest["sha256"][name]:
            raise ValueError(f"{name} hash mismatch")
    for condition, raw_path in prompts.items():
        path = Path(raw_path)
        if not path.is_absolute() or sha256_file(path) != manifest["prompt_hashes"][condition]:
            raise ValueError(f"{condition} prompt hash mismatch")
    return {"valid": True, "source_count": len(sources), "prompt_count": len(prompts)}


@contextmanager
def _ledger_lock(run_dir: Path):
    path = run_dir / ".claims.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a+b") as lock:
        if lock.tell() == 0:
            lock.write(b"0")
            lock.flush()
        lock.seek(0)
        if os.name == "nt":
            import msvcrt
            msvcrt.locking(lock.fileno(), msvcrt.LK_LOCK, 1)
        else:
            import fcntl
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            lock.seek(0)
            if os.name == "nt":
                msvcrt.locking(lock.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(lock.fileno(), fcntl.LOCK_UN)


def _utc(value: datetime, label: str) -> str:
    if value.tzinfo is None or value.utcoffset() is None:
        raise ValueError(f"{label} requires a timezone")
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def _claim_states(run_dir: Path) -> dict[str, dict]:
    dispatch_keys = {row["run_key"] for row in _ledger_rows(run_dir / "dispatch.jsonl")}
    states = {}
    used_threads = set()
    for event in _ledger_rows(run_dir / "claims.jsonl"):
        if set(event) not in ({"run_key", "status", "at"}, {"run_key", "status", "thread_id", "at"}):
            raise ValueError("invalid claim event")
        run_key = event["run_key"]
        status = event["status"]
        previous = states.get(run_key)
        if run_key not in dispatch_keys:
            raise ValueError("claim references unknown run key")
        if status == "claimed" and (previous is None or previous["status"] in {"recovered", "closed"}):
            pass
        elif status == "assigned" and previous and previous["status"] == "claimed":
            thread_id = event.get("thread_id")
            if not isinstance(thread_id, str) or not thread_id or thread_id in used_threads:
                raise ValueError("invalid or duplicate assigned thread")
            used_threads.add(thread_id)
        elif status == "recovered" and previous and previous["status"] == "claimed":
            pass
        elif status == "closed" and previous and previous["status"] == "assigned" and event.get("thread_id") == previous.get("thread_id"):
            pass
        else:
            raise ValueError("invalid claim transition")
        states[run_key] = event
    return states


def claim_runs(
    run_dir: Path,
    controller_inputs: Path,
    limit: int,
    claimed_at: datetime,
) -> list[dict]:
    if limit < 1:
        raise ValueError("limit must be positive")
    at = _utc(claimed_at, "claimed_at")
    verify_inputs(run_dir, controller_inputs)
    verify_results(run_dir)
    with _ledger_lock(run_dir):
        rows = pending_runs(run_dir, limit, controller_inputs=controller_inputs)
        for row in rows:
            _append_jsonl(run_dir / "claims.jsonl", {
                "run_key": row["run_key"], "status": "claimed", "at": at,
            })
        return rows


def assign_claim(
    run_dir: Path, run_key: str, thread_id: str, assigned_at: datetime
) -> dict:
    at = _utc(assigned_at, "assigned_at")
    with _ledger_lock(run_dir):
        states = _claim_states(run_dir)
        if states.get(run_key, {}).get("status") != "claimed":
            raise ValueError("run is not claimed")
        if any(row.get("thread_id") == thread_id for row in _ledger_rows(run_dir / "results.jsonl")):
            raise ValueError("thread is already assigned")
        event = {"run_key": run_key, "status": "assigned", "thread_id": thread_id, "at": at}
        _append_jsonl(run_dir / "claims.jsonl", event)
        return event


def recover_claim(run_dir: Path, run_key: str, recovered_at: datetime) -> dict:
    at = _utc(recovered_at, "recovered_at")
    with _ledger_lock(run_dir):
        states = _claim_states(run_dir)
        status = states.get(run_key, {}).get("status")
        if status == "assigned":
            raise ValueError("assigned claim cannot be recovered")
        if status != "claimed":
            raise ValueError("run is not claimed")
        event = {"run_key": run_key, "status": "recovered", "at": at}
        _append_jsonl(run_dir / "claims.jsonl", event)
        return event


def _chain_row(record: dict, previous: str) -> dict:
    record_sha = hashlib.sha256(_canonical(record)).hexdigest()
    chain_sha = hashlib.sha256((previous + record_sha).encode()).hexdigest()
    return {
        "run_key": record["run_key"],
        "record_sha256": record_sha,
        "previous_chain_sha256": previous,
        "chain_sha256": chain_sha,
    }


def _validate_result_record(record: dict, dispatch: dict[str, dict]) -> None:
    if not isinstance(record, dict) or set(record) != RESULT_KEYS:
        raise ValueError("result record has invalid fields")
    run_key = record["run_key"]
    if run_key not in dispatch:
        raise ValueError(f"unknown run key: {run_key}")
    item = dispatch[run_key]
    for key in ("condition", "repetition", "order", "id", "split"):
        if record[key] != item[key]:
            raise ValueError(f"result {key} does not match dispatch")
    if record["status"] not in RESULT_STATUSES:
        raise ValueError("invalid result status")
    if not isinstance(record["thread_id"], str) or not record["thread_id"]:
        raise ValueError("result requires a non-empty thread ID")
    if not isinstance(record["model"], str) or not record["model"]:
        raise ValueError("invalid result model")
    if not isinstance(record["reasoning"], str) or not record["reasoning"]:
        raise ValueError("invalid result reasoning")
    if not isinstance(record["timeout_seconds"], int) or record["timeout_seconds"] < 1:
        raise ValueError("invalid result timeout")
    completed = _timestamp(record["completed_at"])
    if completed is None or completed.tzinfo is None or completed.utcoffset() is None:
        raise ValueError("invalid result completion timestamp")
    if record["duration_seconds"] is not None and (
        not isinstance(record["duration_seconds"], (int, float))
        or isinstance(record["duration_seconds"], bool)
        or record["duration_seconds"] < 0
    ):
        raise ValueError("invalid result duration")
    if not isinstance(record["violations"], list) or any(not isinstance(value, str) for value in record["violations"]):
        raise ValueError("invalid result violations")
    if set(record["tokens"]) != set(TOKEN_KEYS):
        raise ValueError("invalid result tokens")
    if any(
        value is not None and (
            not isinstance(value, int) or isinstance(value, bool) or value < 0
        )
        for value in record["tokens"].values()
    ):
        raise ValueError("invalid result token value")
    if not isinstance(record["tool_counts"], dict) or any(
        not isinstance(name, str) or not name
        or not isinstance(count, int) or isinstance(count, bool) or count < 0
        for name, count in record["tool_counts"].items()
    ):
        raise ValueError("invalid result tool counts")
    for key in ("ragrep_search_count", "wrapper_ragrep_search_count", "rg_count", "file_read_count"):
        if not isinstance(record[key], int) or isinstance(record[key], bool) or record[key] < 0:
            raise ValueError(f"invalid result {key}")
    if record["worker_output"] is not None:
        parse_worker_output(json.dumps(record["worker_output"], ensure_ascii=False), item["id"], item["condition"])
    expected_trace_keys = {
        "name", "arguments", "output_sha256", "output_excerpt", "output_markers",
        "output_mentions_skill", "output_mentions_ragrep",
    }
    if not isinstance(record["tool_calls"], list):
        raise ValueError("invalid result tool calls")
    for call in record["tool_calls"]:
        if not isinstance(call, dict) or set(call) != expected_trace_keys:
            raise ValueError("invalid result tool call")
        if not isinstance(call["name"], str) or not call["name"]:
            raise ValueError("invalid result tool name")
        if not isinstance(call["output_sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", call["output_sha256"]):
            raise ValueError("invalid result tool output hash")
        if call["output_excerpt"] is not None and (
            not isinstance(call["output_excerpt"], str) or len(call["output_excerpt"]) > 512
        ):
            raise ValueError("invalid result tool output excerpt")
        if not isinstance(call["output_markers"], list) or any(
            marker not in FORBIDDEN_MARKERS for marker in call["output_markers"]
        ):
            raise ValueError("invalid result tool output markers")
        if type(call["output_mentions_skill"]) is not bool or type(call["output_mentions_ragrep"]) is not bool:
            raise ValueError("invalid result tool audit flags")
    if not isinstance(record["source_transcript"], str) or not Path(record["source_transcript"]).is_absolute():
        raise ValueError("invalid result transcript path")
    for key in ("transcript_sha256", "prompt_sha256", "binary_sha256", "db_sha256"):
        if not isinstance(record[key], str) or not re.fullmatch(r"[0-9a-f]{64}", record[key]):
            raise ValueError(f"invalid result {key}")
    if item["condition"] == CONDITIONS[0]:
        if record["skill_sha256"] is not None:
            raise ValueError("C0 result must not carry a skill hash")
    elif not isinstance(record["skill_sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", record["skill_sha256"]):
        raise ValueError("invalid result skill hash")


def verify_results(run_dir: Path) -> dict:
    dispatch = _ledger_rows(run_dir / "dispatch.jsonl")
    dispatch_keys = [row.get("run_key") for row in dispatch]
    if len(dispatch_keys) != len(set(dispatch_keys)) or any(not isinstance(key, str) for key in dispatch_keys):
        raise ValueError("dispatch contains invalid or duplicate run keys")
    expected = set(dispatch_keys)
    by_dispatch = {row["run_key"]: row for row in dispatch}
    results = _ledger_rows(run_dir / "results.jsonl")
    chain = _ledger_rows(run_dir / "results.sha256-chain.jsonl")
    if len(chain) > len(results) or len(results) - len(chain) > 1:
        raise ValueError("results chain length mismatch")
    previous = GENESIS_SHA256
    for index, record in enumerate(results):
        _validate_result_record(record, by_dispatch)
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
    non_infra_threads = [row["thread_id"] for row in valid_rows]
    if len(non_infra_threads) != len(set(non_infra_threads)):
        raise ValueError("duplicate result thread ID")
    archives = _ledger_rows(run_dir / "archive.jsonl")
    result_pairs = {(row["run_key"], row["thread_id"]) for row in results}
    archive_pairs = []
    for row in archives:
        if set(row) != {"run_key", "thread_id", "archived_at"}:
            raise ValueError("archive event has invalid fields")
        pair = (row["run_key"], row["thread_id"])
        if pair not in result_pairs:
            raise ValueError("archive event does not match a result")
        archive_pairs.append(pair)
    if len(archive_pairs) != len(set(archive_pairs)):
        raise ValueError("duplicate archive event")
    archived = set(archive_pairs)
    unarchived = [row["thread_id"] for row in results if (row["run_key"], row["thread_id"]) not in archived]
    return {
        "valid": True,
        "result_count": len(valid_rows),
        "infrastructure_invalid_count": len(results) - len(valid_rows),
        "missing_run_keys": sorted(expected - set(counts)),
        "duplicate_run_keys": duplicates,
        "unarchived_thread_ids": unarchived,
        "archive_ready": len(chain) == len(results),
        "all_archived": not unarchived,
    }


def record_result(
    run_dir: Path,
    run_key: str,
    thread_id: str,
    status: str,
    final_text: str,
    transcript: Path,
    completed_at: datetime,
    *,
    controller_inputs: Path,
) -> dict:
    if status not in RESULT_STATUSES:
        raise ValueError(f"invalid status: {status}")
    completed = _utc(completed_at, "completed_at")
    verify_inputs(run_dir, controller_inputs)
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
    state = _claim_states(run_dir).get(run_key)
    if not state or state.get("status") != "assigned" or state.get("thread_id") != thread_id:
        raise ValueError("result does not match an assigned claim")
    trace = parse_transcript(transcript)
    if trace["thread_id"] != thread_id:
        raise ValueError("transcript thread id mismatch")
    item = dispatch[run_key]
    parsed_output = None
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    violations = audit_transcript(
        trace, item["condition"], frozen_skill_path=manifest.get("skill_path"),
        frozen_wrapper_path=manifest.get("wrapper_path"),
    )
    effective_status = status
    if status == "completed":
        try:
            parsed_output = parse_worker_output(final_text, item["id"], item["condition"])
        except (ValueError, json.JSONDecodeError):
            effective_status = "invalid_output"
    if (
        status == "completed" and violations
        or any(violation.startswith("forbidden") for violation in violations)
    ):
        effective_status = "invalid_leakage"
    wrapper_count = sum(
        1 for call in trace["tool_calls"]
        if (command := _shell_command(call)) is not None
        and manifest.get("wrapper_path")
        and _invokes_path(command, manifest["wrapper_path"], "search")
    )
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
        "completed_at": completed,
        "duration_seconds": trace["duration_seconds"],
        "status": effective_status,
        "worker_output": parsed_output,
        "violations": violations,
        "tokens": trace["tokens"],
        "tool_counts": trace["tool_counts"],
        "ragrep_search_count": wrapper_count,
        "wrapper_ragrep_search_count": wrapper_count,
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
    _append_jsonl(run_dir / "claims.jsonl", {
        "run_key": run_key, "status": "closed", "thread_id": thread_id, "at": completed,
    })
    verified = verify_results(run_dir)
    return {**record, "archive_ready": verified["archive_ready"]}


def pending_runs(
    run_dir: Path,
    limit: int = 8,
    *,
    controller_inputs: Path,
) -> list[dict]:
    if limit < 1:
        raise ValueError("limit must be positive")
    verify_inputs(run_dir, controller_inputs)
    verify_results(run_dir)
    dispatch = _ledger_rows(run_dir / "dispatch.jsonl")
    completed = {
        row["run_key"] for row in _ledger_rows(run_dir / "results.jsonl")
        if row["status"] != "infrastructure_invalid"
    }
    active = {
        run_key for run_key, event in _claim_states(run_dir).items()
        if event["status"] in {"claimed", "assigned"}
    }
    return [row for row in dispatch if row["run_key"] not in completed | active][:limit]


def mark_archived(
    run_dir: Path, run_key: str, thread_id: str, archived_at: datetime
) -> dict:
    archived = _utc(archived_at, "archived_at")
    verified = verify_results(run_dir)
    if not verified["archive_ready"]:
        raise ValueError("result is not archive ready")
    results = _ledger_rows(run_dir / "results.jsonl")
    if not any(row["run_key"] == run_key and row["thread_id"] == thread_id for row in results):
        raise ValueError("archive event does not match a result")
    archives = _ledger_rows(run_dir / "archive.jsonl")
    if any(row.get("thread_id") == thread_id or (
        row.get("run_key") == run_key and row.get("thread_id") == thread_id
    ) for row in archives):
        raise ValueError(f"duplicate archive event: {thread_id}")
    event = {
        "run_key": run_key,
        "thread_id": thread_id,
        "archived_at": archived,
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
    wrapper: Path,
    output: Path,
    controller_inputs: Path,
    pilot_ids: tuple[str, str, str],
    seed: int,
    generated_at: datetime,
    prompt_sources: dict[str, Path],
    repetitions: int = 5,
) -> dict:
    if output.exists() or controller_inputs.exists():
        raise ValueError(f"output already exists: {output if output.exists() else controller_inputs}")
    if generated_at.tzinfo is None or generated_at.utcoffset() is None:
        raise ValueError("generated_at requires a timezone")
    if set(prompt_sources) != set(CONDITIONS):
        raise ValueError("prompt_sources must contain one path per condition")
    sources = {
        "frozen_manifest": frozen_manifest,
        "holdout_questions": holdout_questions,
        "development": development,
        "holdout_gold": holdout_gold,
        "validator": validator,
        "skill": skill,
        "db": db,
        "binary": binary,
        "wrapper": wrapper,
    }
    fixed_hashes = {name: sha256_file(path) for name, path in sources.items()}
    prompt_hashes = {condition: sha256_file(path) for condition, path in prompt_sources.items()}
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
        for order, identifier in enumerate(pilot_ids, start=1)
        for condition in CONDITIONS
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
        "wrapper_path": str(wrapper.resolve()),
    }
    controller_value = {
        "sources": {name: str(path.resolve()) for name, path in sources.items()},
        "prompts": {condition: str(path.resolve()) for condition, path in prompt_sources.items()},
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    controller_inputs.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    controller_written = False
    try:
        _write_jsonl(stage / "questions-only.jsonl", public_questions)
        _write_jsonl(stage / "dispatch.jsonl", dispatch)
        _write_jsonl(stage / "pilot-dispatch.jsonl", pilot)
        controller_temp = controller_inputs.with_name(f".{controller_inputs.name}.{os.getpid()}.tmp")
        _write_json(controller_temp, controller_value)
        manifest["controller_inputs_sha256"] = sha256_file(controller_temp)
        (stage / "manifest.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        controller_temp.rename(controller_inputs)
        controller_written = True
        stage.rename(output)
        return manifest
    finally:
        if stage.exists():
            shutil.rmtree(stage)
        if controller_written and not output.exists() and controller_inputs.exists():
            controller_inputs.unlink()


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
        "validator", "skill", "db", "binary", "wrapper", "output", "controller-inputs",
    ):
        prepare.add_argument(f"--{name}", type=Path, required=True)
    prepare.add_argument("--pilot-id", action="append", required=True)
    prepare.add_argument("--seed", type=int, required=True)
    prepare.add_argument("--generated-at", required=True)
    for condition in CONDITIONS:
        prepare.add_argument(f"--prompt-source-{condition}", type=Path, required=True)
    record = subparsers.add_parser("record")
    record.add_argument("--run-dir", type=Path, required=True)
    record.add_argument("--run-key", required=True)
    record.add_argument("--thread-id", required=True)
    record.add_argument("--status", choices=sorted(RESULT_STATUSES), required=True)
    record.add_argument("--transcript", type=Path, required=True)
    record.add_argument("--completed-at", required=True)
    record.add_argument("--controller-inputs", type=Path, required=True)
    verify = subparsers.add_parser("verify")
    verify.add_argument("--run-dir", type=Path, required=True)
    verify.add_argument("--controller-inputs", type=Path, required=True)
    verify_inputs_parser = subparsers.add_parser("verify-inputs")
    verify_inputs_parser.add_argument("--run-dir", type=Path, required=True)
    verify_inputs_parser.add_argument("--controller-inputs", type=Path, required=True)
    pending = subparsers.add_parser("pending")
    pending.add_argument("--run-dir", type=Path, required=True)
    pending.add_argument("--limit", type=int, default=8)
    pending.add_argument("--controller-inputs", type=Path, required=True)
    claim = subparsers.add_parser("claim")
    claim.add_argument("--run-dir", type=Path, required=True)
    claim.add_argument("--controller-inputs", type=Path, required=True)
    claim.add_argument("--limit", type=int, default=8)
    claim.add_argument("--claimed-at", required=True)
    assign = subparsers.add_parser("assign")
    assign.add_argument("--run-dir", type=Path, required=True)
    assign.add_argument("--run-key", required=True)
    assign.add_argument("--thread-id", required=True)
    assign.add_argument("--assigned-at", required=True)
    recover = subparsers.add_parser("recover")
    recover.add_argument("--run-dir", type=Path, required=True)
    recover.add_argument("--run-key", required=True)
    recover.add_argument("--recovered-at", required=True)
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
            prompt_sources = {
                condition: kwargs[f"prompt_source_{condition}"] for condition in CONDITIONS
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
                wrapper=args.wrapper,
                output=args.output,
                controller_inputs=args.controller_inputs,
                pilot_ids=tuple(args.pilot_id),
                seed=args.seed,
                generated_at=_parse_datetime(args.generated_at),
                prompt_sources=prompt_sources,
            )
            print(f'questions={manifest["question_count"]} runs={manifest["run_count"]} output={args.output}')
            return 0
        if args.command == "record":
            result = record_result(
                args.run_dir, args.run_key, args.thread_id, args.status,
                sys.stdin.read(), args.transcript, _parse_datetime(args.completed_at),
                controller_inputs=args.controller_inputs,
            )
            print(_canonical(result).decode())
            return 0
        if args.command == "verify":
            verify_inputs(args.run_dir, args.controller_inputs)
            print(_canonical(verify_results(args.run_dir)).decode())
            return 0
        if args.command == "verify-inputs":
            print(_canonical(verify_inputs(args.run_dir, args.controller_inputs)).decode())
            return 0
        if args.command == "pending":
            for row in pending_runs(args.run_dir, args.limit, controller_inputs=args.controller_inputs):
                print(_canonical(row).decode())
            return 0
        if args.command == "claim":
            for row in claim_runs(
                args.run_dir, args.controller_inputs, args.limit, _parse_datetime(args.claimed_at)
            ):
                print(_canonical(row).decode())
            return 0
        if args.command == "assign":
            print(_canonical(assign_claim(
                args.run_dir, args.run_key, args.thread_id, _parse_datetime(args.assigned_at)
            )).decode())
            return 0
        if args.command == "recover":
            print(_canonical(recover_claim(
                args.run_dir, args.run_key, _parse_datetime(args.recovered_at)
            )).decode())
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
