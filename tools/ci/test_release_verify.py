import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from release_verify import validate_evaluation, validate_mcp_response, validate_mcp_tools


class ReleaseVerifyTests(unittest.TestCase):
    def complete_report(self):
        metrics = {
            "evidence_recall": 0.9,
            "all_evidence_hit_rate": 0.85,
            "reciprocal_rank": 0.5,
            "nonempty_candidate_rate": 0.1,
            "latency_median_ms": 1.0,
            "latency_p95_ms": 1.0,
            "mean_hits": 1.0,
            "mean_stdout_bytes": 10.0,
            "mean_snippet_chars": 4.0,
        }
        return {"runs": [{"id": "a", "mode": "hybrid", "repetition": 1, "elapsed_ms": 1.0, "hit_count": 1, "stdout_bytes": 10, "snippet_chars": 4, "hits": []}], "aggregate": {"text": dict(metrics), "hybrid": dict(metrics), "categories": {"identifier": {"hybrid": {"all_evidence_hit_rate": 1}}}}}

    def test_evaluation_gate_rejects_known_eighty_percent_result(self):
        report = {"runs": [{"stale": False}], "aggregate": {"hybrid": {"evidence_recall": 0.9, "all_evidence_hit_rate": 0.8, "latency_p95_ms": 1}, "categories": {"identifier": {"hybrid": {"all_evidence_hit_rate": 1}}}}}
        errors = validate_evaluation(report)
        self.assertEqual(errors, ["hybrid all_evidence_hit_rate 0.800 < 0.850"])

    def test_evaluation_gate_accepts_thresholds(self):
        report = self.complete_report()
        self.assertEqual(validate_evaluation(report), [])

    def test_evaluation_gate_rejects_missing_or_nonfinite_required_metric(self):
        report = self.complete_report()
        del report["aggregate"]["hybrid"]["evidence_recall"]
        self.assertIn("hybrid evidence_recall missing", validate_evaluation(report))
        report = self.complete_report()
        report["aggregate"]["hybrid"]["latency_p95_ms"] = float("nan")
        self.assertIn("hybrid latency_p95_ms missing or non-finite", validate_evaluation(report))

    def test_mcp_response_rejects_protocol_error_and_requires_hit(self):
        self.assertIn("protocol error", validate_mcp_response({"error": {}}))
        self.assertIn("missing expected hit", validate_mcp_response({"result": {"content": [{"text": "[]"}]}}))
        self.assertEqual(validate_mcp_response({"result": {"content": [{"text": json.dumps({"data": {"hits": [{"path": "notes.md", "snippet": "needle"}]}})}]}}), [])

    def test_mcp_tools_requires_exact_tool_count(self):
        self.assertIn("expected 9", validate_mcp_tools({"result": {"tools": []}}))
        self.assertEqual(validate_mcp_tools({"result": {"tools": [{} for _ in range(9)]}}), [])


if __name__ == "__main__":
    unittest.main()
