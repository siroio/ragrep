import json
import sys
from pathlib import Path

import unittest

sys.path.insert(0, str(Path(__file__).parent))

from token_usage import collect
import token_usage


def _write(path, records):
    path.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")


class TokenUsageTests(unittest.TestCase):
  def test_accepts_encrypted_child_prompt_with_explicit_identity(self):
    tmp_path = Path(__import__('tempfile').mkdtemp())
    child = tmp_path / "child.jsonl"
    _write(child, [{"type": "session_meta", "payload": {"id": "child", "agent_path": "/root/token_collector", "parent_thread_id": "parent"}}, {"type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"text": "encrypted"}]}}])
    result = collect(child, "MARK", expected_session_id="child", agent_path="/root/token_collector", parent_thread_id="parent")
    self.assertEqual(result["prompt_verification"], "identity_only_encrypted")

  def test_rejects_encrypted_identity_mismatch(self):
    tmp_path = Path(__import__('tempfile').mkdtemp())
    child = tmp_path / "child.jsonl"
    _write(child, [{"type": "session_meta", "payload": {"id": "child", "agent_path": "/root/other", "parent_thread_id": "parent"}}])
    with self.assertRaisesRegex(ValueError, "agent_path"):
        collect(child, "MARK", expected_session_id="child", agent_path="/root/token_collector", parent_thread_id="parent")

  def test_collects_custom_calls_by_call_id_and_separate_last_usage(self):
    tmp_path = Path(__import__('tempfile').mkdtemp())
    session = tmp_path / "session.jsonl"
    _write(session, [
        {"type": "session_meta", "payload": {"session_id": "ROOT", "id": "child-1", "model": "gpt-test"}},
        {"type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"text": "MARK"}]}},
        {"type": "response_item", "payload": {"type": "custom_tool_call", "call_id": "a", "name": "functions.exec", "input": {"cmd": "one"}}},
        {"type": "response_item", "payload": {"type": "custom_tool_call", "call_id": "b", "name": "functions.exec", "input": {"cmd": "two"}}},
        {"type": "response_item", "payload": {"type": "custom_tool_call_output", "call_id": "b", "output": "out-b"}},
        {"type": "response_item", "payload": {"type": "custom_tool_call_output", "call_id": "a", "output": "out-a"}},
        {"type": "event_msg", "payload": {"type": "token_count", "info": {"total_token_usage": {"input_tokens": 5}, "last_token_usage": {"input_tokens": 2}}}},
        {"type": "response_item", "channel": "commentary", "payload": {"type": "message", "role": "assistant", "content": [{"text": "intermediate"}]}},
        {"type": "response_item", "channel": "final", "payload": {"type": "message", "role": "assistant", "content": [{"text": "final"}]}},
    ])
    result = collect(session, "MARK")
    self.assertEqual(result["session_id"], "child-1")
    self.assertEqual(result["last_token_usage"], {"input_tokens": 2})
    self.assertEqual(result["usage"], {"input_tokens": 5})
    self.assertEqual([call["call_id"] for call in result["tool_calls"]], ["a", "b"])
    self.assertEqual(result["tool_calls"][0]["output"], "out-a")
    self.assertEqual(result["tool_calls"][1]["output"], "out-b")
    self.assertEqual(result["final_assistant_text"], "final")

  def test_collects_marked_session_and_uses_last_cumulative_usage(self):
    tmp_path = Path(__import__('tempfile').mkdtemp())
    session = tmp_path / "session.jsonl"
    _write(session, [
        {"timestamp": "2026-09-09T00:00:00Z", "type": "session_meta", "payload": {"id": "s-1", "model": "gpt-test", "effort": "medium"}},
        {"timestamp": "2026-09-09T00:00:01Z", "type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "run MARK-7"}]}},
        {"timestamp": "2026-09-09T00:00:02Z", "type": "response_item", "payload": {"type": "function_call", "name": "exec", "arguments": "{\"cmd\":\"go test\"}"}},
        {"timestamp": "2026-09-09T00:00:03Z", "type": "response_item", "payload": {"type": "function_call_output", "output": "ok"}},
        {"timestamp": "2026-09-09T00:00:04Z", "type": "event_msg", "payload": {"type": "token_count", "info": {"total_token_usage": {"input_tokens": 4, "output_tokens": 2}, "cached_input_tokens": 99}}},
        {"timestamp": "2026-09-09T00:00:05Z", "type": "event_msg", "payload": {"type": "token_count", "info": {"total_token_usage": {"input_tokens": 10, "output_tokens": 8}, "reasoning_tokens": 3}}},
        {"timestamp": "2026-09-09T00:00:06Z", "type": "response_item", "payload": {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "done"}]}},
    ])

    result = collect(session, "MARK-7")

    self.assertEqual(result["session_id"], "s-1")
    self.assertEqual(result["model"], "gpt-test")
    self.assertEqual(result["reasoning_effort"], "medium")
    self.assertEqual(result["usage"], {"input_tokens": 10, "output_tokens": 8})
    self.assertEqual(result["final_assistant_text"], "done")
    self.assertEqual(result["tool_call_count"], 1)
    self.assertEqual(result["tool_calls"][0]["name"], "exec")
    self.assertEqual(result["tool_calls"][0]["input"], {"cmd": "go test"})
    self.assertEqual(result["tool_calls"][0]["output"], "ok")
    self.assertEqual(result["tool_output_utf8_bytes"], len("ok".encode("utf-8")))
    self.assertLess(result["started_at"], result["finished_at"])


  def test_marker_mismatch_writes_nothing(self):
    tmp_path = Path(__import__('tempfile').mkdtemp())
    session = tmp_path / "session.jsonl"
    output = tmp_path / "out.json"
    _write(session, [{"type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"text": "other"}]}}])
    with self.assertRaisesRegex(ValueError, "marker"):
        collect(session, "MARK", output)
    self.assertFalse(output.exists())


  def test_cli_refuses_to_overwrite_output(self):
    tmp_path = Path(__import__('tempfile').mkdtemp())
    session = tmp_path / "session.jsonl"
    output = tmp_path / "out.json"
    _write(session, [{"type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"text": "MARK"}]}}])
    output.write_text("keep", encoding="utf-8")
    with self.assertRaisesRegex(ValueError, "already exists"):
        collect(session, "MARK", output)
    self.assertEqual(output.read_text(encoding="utf-8"), "keep")


if __name__ == "__main__":
    unittest.main()
