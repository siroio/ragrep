"""Prepare and gate blinded, rubric-based answer regression checks.

This module deliberately does not infer semantic correctness from answer text.
An independent human or LLM judge supplies the structured grade for each
blinded case; this module validates that grade and compares it with the case's
held-out expected outcome.
"""

import argparse
import copy
import json
from pathlib import Path


GRADE_FIELDS = (
    "required_points_complete",
    "source_action_qualifier_fidelity",
    "language_matches_query",
    "correct_abstention",
    "citations_valid",
)
KNOWN_VIOLATIONS = frozenset((*GRADE_FIELDS, "unsupported_claims"))
BLIND_FIELDS = ("id", "query", "language", "answerable", "passages", "rubric", "answer")
OUTCOMES = {"pass", "regression"}


def _nonempty_string(value, field):
    if not isinstance(value, str) or not value.strip():
        raise ValueError(field)


def _validate_answer(answer, fixture_id):
    if not isinstance(answer, dict):
        raise ValueError("answer")
    if answer.get("id") != fixture_id:
        raise ValueError("answer id")
    _nonempty_string(answer.get("answer"), "answer text")
    if not isinstance(answer.get("abstain"), bool):
        raise ValueError("answer abstain")
    citations = answer.get("citations")
    if not isinstance(citations, list):
        raise ValueError("answer citations")
    for citation in citations:
        if not isinstance(citation, dict):
            raise ValueError("citation")
        _nonempty_string(citation.get("doc"), "citation doc")
        if not isinstance(citation.get("para"), int) or isinstance(citation["para"], bool) or citation["para"] < 0:
            raise ValueError("citation para")


def _validate_fixture(fixture):
    if not isinstance(fixture, dict):
        raise ValueError("fixture")
    fixture_id = fixture.get("id")
    _nonempty_string(fixture_id, "id")
    _nonempty_string(fixture.get("query"), "query")
    _nonempty_string(fixture.get("language"), "language")
    if not isinstance(fixture.get("answerable"), bool):
        raise ValueError("answerable")
    passages = fixture.get("passages")
    if not isinstance(passages, list) or not passages:
        raise ValueError("passages")
    for passage in passages:
        if not isinstance(passage, dict):
            raise ValueError("passage")
        _nonempty_string(passage.get("doc"), "passage doc")
        if not isinstance(passage.get("para"), int) or isinstance(passage["para"], bool) or passage["para"] < 0:
            raise ValueError("passage para")
        _nonempty_string(passage.get("body"), "passage body")
    rubric = fixture.get("rubric")
    if not isinstance(rubric, dict):
        raise ValueError("rubric")
    required_points = rubric.get("required_points")
    if not isinstance(required_points, list) or not required_points or any(not isinstance(point, str) or not point.strip() for point in required_points):
        raise ValueError("rubric required_points")
    for field in ("source_action_qualifier_fidelity", "language_matches_query", "correct_abstention", "citations_valid"):
        _nonempty_string(rubric.get(field), f"rubric {field}")
    _validate_answer(fixture.get("answer"), fixture_id)
    if fixture.get("expected_outcome") not in OUTCOMES:
        raise ValueError("expected_outcome")
    expected_violations = fixture.get("expected_violations")
    if not isinstance(expected_violations, list) or any(not isinstance(item, str) or not item.strip() for item in expected_violations):
        raise ValueError("expected_violations")
    if any(item not in KNOWN_VIOLATIONS for item in expected_violations):
        raise ValueError("expected_violations")
    if fixture["expected_outcome"] == "regression" and not expected_violations:
        raise ValueError("expected_violations")
    if fixture["expected_outcome"] == "pass" and expected_violations:
        raise ValueError("expected_violations")
    _nonempty_string(fixture.get("provenance"), "provenance")
    _nonempty_string(fixture.get("source_ref"), "source_ref")


def load_fixtures_from_records(records):
    fixtures, ids = [], set()
    for fixture in records:
        _validate_fixture(fixture)
        if fixture["id"] in ids:
            raise ValueError("duplicate id")
        ids.add(fixture["id"])
        fixtures.append(fixture)
    if not fixtures:
        raise ValueError("zero fixtures")
    return fixtures


def load_fixtures(path):
    """Load one JSON object per non-empty line and validate its held-out labels."""
    path = Path(path)
    records = []
    for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not line.strip():
            continue
        try:
            records.append(json.loads(line))
        except json.JSONDecodeError as exc:
            raise ValueError(f"malformed JSON line {line_number}") from exc
    return load_fixtures_from_records(records)


def blind_payload(fixtures):
    """Return judge input without the expected outcome or provenance labels."""
    return {"cases": [{field: copy.deepcopy(fixture[field]) for field in BLIND_FIELDS} for fixture in fixtures]}


def _validate_grade(grade, fixture_ids):
    if not isinstance(grade, dict):
        raise ValueError("grade")
    grade_id = grade.get("id")
    if grade_id not in fixture_ids:
        raise ValueError("unknown grade id")
    for field in GRADE_FIELDS:
        if not isinstance(grade.get(field), bool):
            raise ValueError(field)
    unsupported = grade.get("unsupported_claims")
    if not isinstance(unsupported, list) or any(not isinstance(item, str) for item in unsupported):
        raise ValueError("unsupported_claims")
    _nonempty_string(grade.get("reason"), "reason")


def _grade_passes(grade):
    return all(grade[field] for field in GRADE_FIELDS) and not grade["unsupported_claims"]


def _grade_violations(grade):
    violations = [field for field in GRADE_FIELDS if not grade[field]]
    if grade["unsupported_claims"]:
        violations.append("unsupported_claims")
    return violations


def load_grades(path):
    """Load grades as JSON, or as JSONL for line-oriented judge output."""
    text = Path(path).read_text(encoding="utf-8")
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        parsed = [json.loads(line) for line in text.splitlines() if line.strip()]
    if isinstance(parsed, dict):
        parsed = parsed.get("grades")
    if not isinstance(parsed, list):
        raise ValueError("grades")
    return parsed


def evaluate_grades(fixtures, grades):
    """Compare independent structured grades with held-out fixture outcomes."""
    fixture_by_id = {fixture["id"]: fixture for fixture in fixtures}
    if len(fixture_by_id) != len(fixtures):
        raise ValueError("duplicate fixture id")
    seen = set()
    results = []
    for grade in grades:
        _validate_grade(grade, fixture_by_id)
        if grade["id"] in seen:
            raise ValueError("duplicate grade id")
        seen.add(grade["id"])
        fixture = fixture_by_id[grade["id"]]
        observed_pass = _grade_passes(grade)
        expected_pass = fixture["expected_outcome"] == "pass"
        actual_violations = set(_grade_violations(grade))
        expected_violations = set(fixture["expected_violations"])
        matches_expected = observed_pass == expected_pass
        if not expected_pass:
            matches_expected = not observed_pass and expected_violations.issubset(actual_violations)
        results.append(
            {
                "id": grade["id"],
                "expected_outcome": fixture["expected_outcome"],
                "expected_violations": fixture["expected_violations"],
                "observed_pass": observed_pass,
                "violations": _grade_violations(grade),
                "matches_expected": matches_expected,
                "reason": grade["reason"],
            }
        )
    missing = set(fixture_by_id) - seen
    if missing:
        raise ValueError("missing grade id")
    passed = sum(item["expected_outcome"] == "pass" and item["observed_pass"] for item in results)
    regressions_detected = sum(item["expected_outcome"] == "regression" and not item["observed_pass"] for item in results)
    return {
        "gate_pass": all(item["matches_expected"] for item in results),
        "summary": {"cases": len(results), "passed": passed, "regressions_detected": regressions_detected},
        "cases": results,
    }


def _write_json(path, value):
    with Path(path).open("x", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write("\n")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    prepare = subparsers.add_parser("prepare")
    prepare.add_argument("--fixtures", required=True)
    prepare.add_argument("--output", required=True)
    gate = subparsers.add_parser("gate")
    gate.add_argument("--fixtures", required=True)
    gate.add_argument("--grades", required=True)
    gate.add_argument("--output", required=True)
    args = parser.parse_args(argv)
    fixtures = load_fixtures(args.fixtures)
    if args.command == "prepare":
        _write_json(args.output, blind_payload(fixtures))
        return 0
    result = evaluate_grades(fixtures, load_grades(args.grades))
    _write_json(args.output, result)
    return 0 if result["gate_pass"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
