"""Collect measured token usage from one explicitly supplied Codex JSONL session."""

import argparse
import datetime as dt
import json
import os
from pathlib import Path


def _texts(value):
    if isinstance(value, str):
        return [value]
    if isinstance(value, list):
        return [part for item in value for part in _texts(item)]
    if isinstance(value, dict):
        return _texts(value.get("text", ""))
    return []


def _timestamp(record):
    for key in ("timestamp", "created_at", "time"):
        value = record.get(key)
        if isinstance(value, (str, int, float)):
            return value
    return None


def _seconds(value):
    if isinstance(value, (int, float)):
        return float(value)
    if not isinstance(value, str):
        return None
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def _records(path):
    records = []
    with Path(path).open(encoding="utf-8") as stream:
        for line_number, line in enumerate(stream, 1):
            if not line.strip():
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(f"malformed JSON at line {line_number}") from exc
            if isinstance(record, dict):
                records.append(record)
    return records


def collect(session, marker, output=None, expected_session_id=None, agent_path=None, parent_thread_id=None):
    if not marker:
        raise ValueError("marker is required")
    records = _records(session)

    user_texts = []
    assistant_text = ""
    usage = None
    session_id = model = reasoning_effort = None
    child_session_id = None
    calls, calls_by_id = [], {}
    pending = None
    last_usage = None
    final_assistant_text = ""
    output_bytes = 0
    for record in records:
        payload = record.get("payload", record)
        if not isinstance(payload, dict):
            payload = record
        child_session_id = child_session_id or payload.get("id") if payload.get("session_id") else child_session_id
        session_id = session_id or record.get("session_id") or payload.get("session_id")
        model = model or record.get("model") or payload.get("model")
        reasoning_effort = reasoning_effort or record.get("reasoning_effort") or payload.get("reasoning_effort") or record.get("effort") or payload.get("effort")
        kind = payload.get("type")
        if record.get("type") == "session_meta" or kind == "session_meta":
            child_session_id = child_session_id or payload.get("id")
        if kind == "message":
            text = "".join(_texts(payload.get("content", payload.get("text", ""))))
            if payload.get("role") == "user":
                user_texts.append(text)
            elif payload.get("role") == "assistant":
                assistant_text = text
                if record.get("channel") == "final" or payload.get("channel") == "final":
                    final_assistant_text = text
        if kind in {"function_call", "tool_call", "custom_tool_call"}:
            raw = payload.get("arguments", payload.get("input"))
            try:
                raw = json.loads(raw) if isinstance(raw, str) else raw
            except json.JSONDecodeError:
                pass
            call = {"name": payload.get("name") or payload.get("function", ""), "input": raw, "output": None}
            if payload.get("call_id") is not None:
                call["call_id"] = payload["call_id"]
            calls.append(call)
            pending = call
            if payload.get("call_id") is not None:
                calls_by_id[payload["call_id"]] = call
        elif kind in {"function_call_output", "tool_output", "custom_tool_call_output"}:
            value = payload.get("output", payload.get("result", ""))
            call = calls_by_id.get(payload.get("call_id")) or pending
            if call is not None:
                call["output"] = value
            output_bytes += len(str(value).encode("utf-8"))
        info = payload.get("info") if payload.get("type") == "token_count" else None
        if isinstance(info, dict):
            if isinstance(info.get("total_token_usage"), dict):
                usage = dict(info["total_token_usage"])
            if isinstance(info.get("last_token_usage"), dict):
                last_usage = dict(info["last_token_usage"])

    direct_marker = any(marker in text for text in user_texts)
    prompt_verification = "marker_verified"
    if not direct_marker:
        if not expected_session_id or not agent_path or not parent_thread_id:
            raise ValueError("marker not found in user prompt")
        child_meta = next((r.get("payload", {}) for r in records if r.get("type") == "session_meta"), {})
        if child_meta.get("agent_path") != agent_path:
            raise ValueError("agent_path mismatch")
        if child_meta.get("id") != expected_session_id:
            raise ValueError("session id mismatch")
        if child_meta.get("parent_thread_id") != parent_thread_id:
            raise ValueError("parent session mismatch")
        prompt_verification = "identity_only_encrypted"
    timestamps = [_timestamp(record) for record in records if _timestamp(record) is not None]
    started = timestamps[0] if timestamps else None
    finished = timestamps[-1] if timestamps else None
    start_seconds, finish_seconds = _seconds(started), _seconds(finished)
    result = {
        "session_id": child_session_id or session_id,
        "model": model,
        "reasoning_effort": reasoning_effort,
        "started_at": started,
        "finished_at": finished,
        "wall_time_seconds": finish_seconds - start_seconds if start_seconds is not None and finish_seconds is not None else None,
        "final_assistant_text": final_assistant_text or assistant_text,
        "prompt_verification": prompt_verification,
        "usage": usage,
        "total_token_usage": usage,
        "last_token_usage": last_usage,
        "tool_calls": calls,
        "tool_call_count": len(calls),
        "tool_output_utf8_bytes": output_bytes,
    }
    if output is not None:
        path = Path(output)
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        try:
            fd = os.open(path, flags)
        except FileExistsError as exc:
            raise ValueError("output already exists") from exc
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(result, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
    return result


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--session", required=True)
    parser.add_argument("--marker", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--expected-session-id")
    parser.add_argument("--agent-path")
    parser.add_argument("--parent-thread-id")
    args = parser.parse_args(argv)
    try:
        collect(args.session, args.marker, args.output, args.expected_session_id, args.agent_path, args.parent_thread_id)
    except (OSError, ValueError) as exc:
        parser.error(str(exc))


if __name__ == "__main__":
    main()
