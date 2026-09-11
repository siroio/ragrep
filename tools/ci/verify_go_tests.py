#!/usr/bin/env python3
"""Fail a required real-test gate when tests are missing, skipped, or failing."""

import argparse
import json
import sys


def verify(lines, required):
    states = {name: [] for name in required}
    for line in lines:
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        name = event.get("Test")
        if name in states and event.get("Action") in {"pass", "fail", "skip"}:
            states[name].append(event["Action"])
    errors = []
    for name, actions in states.items():
        if "fail" in actions:
            errors.append(f"{name}: failed")
        elif "skip" in actions:
            errors.append(f"{name}: skipped")
        elif "pass" not in actions:
            errors.append(f"{name}: missing")
    return errors


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", help="go test -json output; stdin when omitted")
    parser.add_argument("--required", action="append", required=True)
    args = parser.parse_args(argv)
    stream = open(args.input, encoding="utf-8") if args.input else sys.stdin
    try:
        errors = verify(stream, args.required)
    finally:
        if args.input:
            stream.close()
    if errors:
        print("required Go tests: " + "; ".join(errors), file=sys.stderr)
        return 1
    print(f"required Go tests: {len(args.required)} passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
