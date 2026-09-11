#!/usr/bin/env python3
"""Checks artifacts produced by the opt-in, real-environment release job."""

import argparse
import json
import math
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from verify_go_tests import verify


def _finite_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def validate_evaluation(report):
    aggregate = report.get("aggregate", {})
    hybrid = aggregate.get("hybrid", {})
    text = aggregate.get("text", {})
    errors = []
    runs = report.get("runs", [])
    if not runs:
        errors.append("evaluation has no runs")
    if any(not isinstance(run, dict) or run.get("stale") for run in runs):
        errors.append("evaluation contains stale or invalid runs")
    for key, minimum in (("evidence_recall", 0.90), ("all_evidence_hit_rate", 0.85)):
        value = hybrid.get(key)
        if not _finite_number(value):
            shown = "missing" if value is None else "missing or non-finite"
            errors.append(f"hybrid {key} {shown}")
        elif value < minimum:
            shown = f"{value:.3f}"
            errors.append(f"hybrid {key} {shown} < {minimum:.3f}")
    p95 = hybrid.get("latency_p95_ms")
    if not _finite_number(p95):
        errors.append("hybrid latency_p95_ms missing or non-finite")
    elif p95 > 1000:
        errors.append(f"hybrid latency_p95_ms {p95:.1f} > 1000.0")
    identifier = aggregate.get("categories", {}).get("identifier", {}).get("hybrid", {})
    if identifier.get("all_evidence_hit_rate") != 1:
        errors.append("hybrid identifier all_evidence_hit_rate is below 1.000")
    if _finite_number(text.get("evidence_recall")) and _finite_number(hybrid.get("evidence_recall")):
        if hybrid["evidence_recall"] + 0.05 < text["evidence_recall"]:
            errors.append("hybrid evidence_recall regressed by more than 0.050")
    return errors


def validate_mcp_response(response):
    if not isinstance(response, dict) or "error" in response or not isinstance(response.get("result"), dict):
        return "protocol error"
    result = response["result"]
    if result.get("isError"):
        return "protocol error: result.isError"
    data = result.get("structuredContent", {}).get("data", {})
    if not isinstance(data, dict):
        data = {}
    if not data:
        for item in result.get("content", []):
            if not isinstance(item, dict) or not isinstance(item.get("text"), str):
                continue
            try:
                content = json.loads(item["text"])
            except json.JSONDecodeError:
                continue
            if isinstance(content, dict) and isinstance(content.get("data"), dict):
                data = content["data"]
                break
    hits = data.get("hits", []) if isinstance(data, dict) else []
    if any(isinstance(hit, dict) and hit.get("path") == "notes.md" and "needle" in hit.get("snippet", "") for hit in hits):
        return []
    return "missing expected hit"


def validate_mcp_tools(response):
    if not isinstance(response, dict) or "error" in response or not isinstance(response.get("result"), dict):
        return "MCP tools/list protocol error"
    tools = response["result"].get("tools")
    if not isinstance(tools, list) or len(tools) != 9:
        return "MCP tools/list expected 9 tools"
    return []


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--go-json", required=True)
    parser.add_argument("--eval-report", required=True)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--mcp-response", required=True)
    parser.add_argument("--mcp-tools-response", required=True)
    parser.add_argument("--required", action="append", required=True)
    args = parser.parse_args(argv)
    errors = []
    if not Path(args.binary).is_file():
        errors.append(f"binary missing: {args.binary}")
    errors.extend(verify(Path(args.go_json).read_text(encoding="utf-8").splitlines(), args.required))
    errors.extend(validate_evaluation(json.loads(Path(args.eval_report).read_text(encoding="utf-8"))))
    mcp_error = validate_mcp_response(json.loads(Path(args.mcp_response).read_text(encoding="utf-8")))
    if mcp_error:
        errors.append(mcp_error)
    tools_error = validate_mcp_tools(json.loads(Path(args.mcp_tools_response).read_text(encoding="utf-8")))
    if tools_error:
        errors.append(tools_error)
    if errors:
        print("release verification: " + "; ".join(errors), file=sys.stderr)
        return 1
    print("release verification: passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
