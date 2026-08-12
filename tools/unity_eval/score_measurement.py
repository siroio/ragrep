import argparse
from collections import Counter
import csv
from datetime import datetime, timezone
import json
from pathlib import Path, PurePosixPath, PureWindowsPath
import random
import shutil
import statistics
import sys
import tempfile

from measure_cases import (
    _append_jsonl, _canonical, _chain_row, _parse_datetime, GENESIS_SHA256,
)
from validate_cases import load_jsonl


GOLD_KEYS = (
    "answer", "required_points", "evidence", "type", "domain", "split",
    "answerable", "unanswerable_kind",
)
GRADE_KEYS = {
    "run_key", "correct", "required_points", "contradiction", "unsupported_claim",
    "unanswerable_correct", "failure_class", "reason", "gold_evidence",
}
FAILURE_CLASSES = {
    "search_failure", "selection_failure", "reading_failure", "unanswerable_failure",
    "invalid_output", "timeout_or_error",
}


def make_grading_batches(
    results: list[dict], gold: list[dict], batch_size: int = 25
) -> list[dict]:
    if batch_size < 1:
        raise ValueError("batch_size must be positive")
    by_id = {}
    for case in gold:
        identifier = case.get("id")
        if not isinstance(identifier, str) or identifier in by_id:
            raise ValueError("gold IDs must be unique strings")
        if any(key not in case for key in GOLD_KEYS):
            raise ValueError(f"{identifier}: incomplete gold record")
        by_id[identifier] = case
    items = []
    seen = set()
    for result in sorted(results, key=lambda row: row["run_key"]):
        run_key = result.get("run_key")
        identifier = result.get("id")
        if run_key in seen or identifier not in by_id:
            raise ValueError("results require unique run keys and matching gold")
        seen.add(run_key)
        items.append({
            "run_key": run_key,
            "result": {key: result.get(key) for key in ("run_key", "id", "status", "worker_output")},
            "gold": {key: by_id[identifier][key] for key in GOLD_KEYS},
        })
    used_ids = {item["result"]["id"] for item in items}
    if used_ids != set(by_id):
        raise ValueError("gold and results are not one-to-one by question ID")
    return [
        {"batch": index // batch_size + 1, "items": items[index:index + batch_size]}
        for index in range(0, len(items), batch_size)
    ]


def _valid_doc(value):
    if not isinstance(value, str) or not value.strip():
        return False
    return (
        not PurePosixPath(value).is_absolute()
        and not PureWindowsPath(value).is_absolute()
        and ".." not in PurePosixPath(value).parts
    )


def validate_grade(
    record: dict,
    expected_run_key: str,
    *,
    expected_points: list[str] | None = None,
    answerable: bool | None = None,
) -> dict:
    if not isinstance(record, dict) or set(record) != GRADE_KEYS:
        raise ValueError("grade has invalid fields")
    if record["run_key"] != expected_run_key:
        raise ValueError("grade run key mismatch")
    for key in ("correct", "contradiction", "unsupported_claim"):
        if type(record[key]) is not bool:
            raise ValueError(f"{key} must be a boolean")
    points = record["required_points"]
    if not isinstance(points, list) or any(
        not isinstance(point, dict) or set(point) != {"point", "met"}
        or not isinstance(point["point"], str) or not point["point"].strip()
        or type(point["met"]) is not bool
        for point in points
    ):
        raise ValueError("required_points is invalid")
    point_names = [point["point"] for point in points]
    if len(point_names) != len(set(point_names)):
        raise ValueError("required points must be unique")
    if expected_points is not None and point_names != expected_points:
        raise ValueError("required points mismatch")
    if record["failure_class"] is not None and record["failure_class"] not in FAILURE_CLASSES:
        raise ValueError("invalid failure class")
    if not isinstance(record["reason"], str) or not record["reason"].strip() or len(record["reason"]) > 500:
        raise ValueError("reason must contain 1 to 500 characters")
    evidence = record["gold_evidence"]
    if not isinstance(evidence, list):
        raise ValueError("gold_evidence must be an array")
    for item in evidence:
        if (
            not isinstance(item, dict) or set(item) != {"doc", "para"}
            or not _valid_doc(item["doc"])
            or not isinstance(item["para"], int) or isinstance(item["para"], bool)
            or item["para"] < 0
        ):
            raise ValueError("invalid gold evidence")
    if record["correct"] and (
        any(not point["met"] for point in points)
        or record["contradiction"] or record["unsupported_claim"]
        or record["failure_class"] is not None
    ):
        raise ValueError("correct grade conflicts with failure details")
    if not record["correct"] and record["failure_class"] is None:
        raise ValueError("incorrect grade requires a failure class")
    if answerable is True and record["unanswerable_correct"] is not None:
        raise ValueError("answerable grade must use null unanswerable_correct")
    if answerable is False:
        if type(record["unanswerable_correct"]) is not bool or points:
            raise ValueError("unanswerable grade has invalid shape")
        if record["unanswerable_correct"] != record["correct"]:
            raise ValueError("unanswerable verdict conflicts with correctness")
    elif answerable is None and record["unanswerable_correct"] not in (None, True, False):
        raise ValueError("invalid unanswerable_correct")
    return record


def compare_grades(a: list[dict], b: list[dict]) -> dict:
    by_a = {grade.get("run_key"): grade for grade in a}
    by_b = {grade.get("run_key"): grade for grade in b}
    if len(by_a) != len(a) or len(by_b) != len(b) or set(by_a) != set(by_b):
        raise ValueError("grade ledgers must contain the same unique run keys")
    agreed = []
    needs = []
    audit = []
    records = []
    for run_key in sorted(by_a):
        left = validate_grade(by_a[run_key], run_key)
        right = validate_grade(by_b[run_key], run_key)
        same = (
            left["correct"] == right["correct"]
            and left["failure_class"] == right["failure_class"]
        )
        status = "agreed" if same else "needs_adjudication"
        (agreed if same else needs).append(run_key)
        if (
            not left["correct"] or not right["correct"]
            or left["unanswerable_correct"] is not None
            or right["unanswerable_correct"] is not None
        ):
            audit.append(run_key)
        records.append({"run_key": run_key, "status": status})
    return {
        "records": records,
        "agreed": agreed,
        "needs_adjudication": needs,
        "audit_run_keys": audit,
    }


def _rate(rows, correct):
    return sum(1 for row in rows if correct[row["run_key"]]) / len(rows) if rows else None


def _breakdown(results, correct, key):
    values = sorted({row[key] for row in results})
    return {
        value: {
            "count": len(group := [row for row in results if row[key] == value]),
            "accuracy": _rate(group, correct),
        }
        for value in values
    }


def _stats(values):
    values = [value for value in values if value is not None]
    if not values:
        return {"mean": None, "median": None, "pstdev": None}
    return {
        "mean": statistics.mean(values),
        "median": statistics.median(values),
        "pstdev": statistics.pstdev(values),
    }


def _bootstrap_accuracy(results, correct, seed, samples=1000):
    groups = {}
    for row in results:
        groups.setdefault(row["id"], []).append(row)
    identifiers = sorted(groups)
    if not identifiers:
        return [None, None]
    rng = random.Random(seed)
    estimates = []
    for _ in range(samples):
        sample = [row for _ in identifiers for row in groups[rng.choice(identifiers)]]
        estimates.append(_rate(sample, correct))
    estimates.sort()
    return [estimates[int(0.025 * (samples - 1))], estimates[int(0.975 * (samples - 1))]]


def aggregate(results: list[dict], grades: list[dict], seed: int) -> dict:
    by_grade = {grade.get("run_key"): grade for grade in grades}
    if len(by_grade) != len(grades) or {row.get("run_key") for row in results} != set(by_grade):
        raise ValueError("every result requires exactly one final grade")
    correct = {key: validate_grade(grade, key)["correct"] for key, grade in by_grade.items()}
    by_condition = _breakdown(results, correct, "condition")
    c0_accuracy = by_condition["c0_no_skill_no_ragrep"]["accuracy"]
    deltas = {
        condition: data["accuracy"] - c0_accuracy
        for condition, data in by_condition.items()
        if condition != "c0_no_skill_no_ragrep"
    }
    indexed = {(row["condition"], row["repetition"], row["id"]): row for row in results}
    regressions = 0
    for row in results:
        if row["condition"] != "c0_no_skill_no_ragrep" or not correct[row["run_key"]]:
            continue
        for condition in ("c1_ragrep_no_skill", "c2_skill_ragrep"):
            peer = indexed.get((condition, row["repetition"], row["id"]))
            if peer is not None and not correct[peer["run_key"]]:
                regressions += 1
    retrieval_rows = [
        row for row in results
        if row["condition"] != "c0_no_skill_no_ragrep"
        and row.get("answerable") and by_grade[row["run_key"]]["gold_evidence"]
    ]
    acquired = {}
    recall = {5: {}, 10: {}}
    for row in retrieval_rows:
        gold = {(item["doc"], item["para"]) for item in by_grade[row["run_key"]]["gold_evidence"]}
        hits = [
            hit for retrieval in row["worker_output"].get("retrievals", [])
            for hit in retrieval.get("hits", [])
        ]
        acquired[row["run_key"]] = any((hit["doc"], hit["para"]) in gold for hit in hits)
        for cutoff in recall:
            recall[cutoff][row["run_key"]] = any(
                hit["rank"] <= cutoff and (hit["doc"], hit["para"]) in gold for hit in hits
            )
    acquired_rows = [row for row in retrieval_rows if acquired[row["run_key"]]]
    retrieval_by_condition = {}
    for condition in ("c1_ragrep_no_skill", "c2_skill_ragrep"):
        condition_rows = [row for row in retrieval_rows if row["condition"] == condition]
        condition_acquired = [row for row in condition_rows if acquired[row["run_key"]]]
        retrieval_by_condition[condition] = {
            "recall_at_5": statistics.mean(recall[5][row["run_key"]] for row in condition_rows) if condition_rows else None,
            "recall_at_10": statistics.mean(recall[10][row["run_key"]] for row in condition_rows) if condition_rows else None,
            "evidence_acquired_rate": statistics.mean(acquired[row["run_key"]] for row in condition_rows) if condition_rows else None,
            "accuracy_given_evidence": _rate(condition_acquired, correct),
        }
    token_keys = (
        "input_tokens", "cached_input_tokens", "output_tokens",
        "reasoning_output_tokens", "total_tokens",
    )
    unanswerable = [row for row in results if not row.get("answerable")]
    return {
        "status": "provisional",
        "overall_accuracy": _rate(results, correct),
        "holdout_accuracy": _rate([row for row in results if row["split"] == "holdout"], correct),
        "unanswerable_rejection_rate": (
            sum(bool(by_grade[row["run_key"]]["unanswerable_correct"]) for row in unanswerable) / len(unanswerable)
            if unanswerable else None
        ),
        "by_condition": by_condition,
        "condition_delta_vs_c0": deltas,
        "c0_correct_retrieval_incorrect_count": regressions,
        "by_split": _breakdown(results, correct, "split"),
        "by_type": _breakdown(results, correct, "type"),
        "by_domain": _breakdown(results, correct, "domain"),
        "failure_class_counts": dict(sorted(Counter(
            grade["failure_class"] for grade in grades if grade["failure_class"] is not None
        ).items())),
        "retrieval": {
            "recall_at_5": statistics.mean(recall[5].values()) if recall[5] else None,
            "recall_at_10": statistics.mean(recall[10].values()) if recall[10] else None,
            "evidence_acquired_rate": statistics.mean(acquired.values()) if acquired else None,
            "accuracy_given_evidence": _rate(acquired_rows, correct),
        },
        "retrieval_by_condition": retrieval_by_condition,
        "duration_seconds": _stats(row.get("duration_seconds") for row in results),
        "tokens": {
            key: _stats(row.get("tokens", {}).get(key) for row in results) for key in token_keys
        },
        "accuracy_bootstrap_95": _bootstrap_accuracy(results, correct, seed),
    }


def prepare_grading(results: list[dict], gold: list[dict], output: Path, batch_size: int = 25):
    if output.exists():
        raise ValueError(f"output already exists: {output}")
    batches = make_grading_batches(results, gold, batch_size)
    output.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    try:
        (stage / "grading-input.jsonl").write_text(
            "".join(_canonical(batch).decode() + "\n" for batch in batches),
            encoding="utf-8",
        )
        stage.rename(output)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    return batches


def _grading_items(run_dir: Path):
    items = {}
    for batch in load_jsonl(run_dir / "grading-input.jsonl"):
        for item in batch.get("items", []):
            run_key = item.get("run_key")
            if not isinstance(run_key, str) or run_key in items:
                raise ValueError("grading input has duplicate or invalid run keys")
            items[run_key] = item
    return items


def verify_grade_ledger(run_dir: Path, ledger: str):
    if ledger not in {"grades-a", "grades-b", "adjudication"}:
        raise ValueError("invalid grade ledger")
    records = load_jsonl(run_dir / f"{ledger}.jsonl") if (run_dir / f"{ledger}.jsonl").exists() else []
    chain_path = run_dir / f"{ledger}.sha256-chain.jsonl"
    chain = load_jsonl(chain_path) if chain_path.exists() else []
    if len(chain) > len(records) or len(records) - len(chain) > 1:
        raise ValueError("grade chain length mismatch")
    previous = GENESIS_SHA256
    seen = set()
    for index, record in enumerate(records):
        if record["run_key"] in seen:
            raise ValueError("duplicate grade")
        seen.add(record["run_key"])
        wanted = _chain_row(record, previous)
        if index < len(chain):
            if chain[index] != wanted:
                raise ValueError("grade chain mismatch")
        else:
            _append_jsonl(chain_path, wanted)
        previous = wanted["chain_sha256"]
    return {"valid": True, "count": len(records)}


def record_grade(run_dir: Path, ledger: str, record: dict):
    items = _grading_items(run_dir)
    run_key = record.get("run_key")
    if ledger == "adjudication":
        comparison = compare_grades(
            load_jsonl(run_dir / "grades-a.jsonl"), load_jsonl(run_dir / "grades-b.jsonl")
        )
        if run_key not in comparison["needs_adjudication"]:
            raise ValueError("adjudication is allowed only for a listed disagreement")
    elif ledger not in {"grades-a", "grades-b"}:
        raise ValueError("invalid grade ledger")
    if run_key not in items:
        raise ValueError("grade run key is absent from grading input")
    gold = items[run_key]["gold"]
    validate_grade(
        record, run_key, expected_points=list(gold["required_points"]),
        answerable=gold["answerable"],
    )
    path = run_dir / f"{ledger}.jsonl"
    existing = load_jsonl(path) if path.exists() else []
    if any(row["run_key"] == run_key for row in existing):
        raise ValueError("duplicate grade")
    verify_grade_ledger(run_dir, ledger)
    _append_jsonl(path, record)
    previous = GENESIS_SHA256
    chain_path = run_dir / f"{ledger}.sha256-chain.jsonl"
    chain = load_jsonl(chain_path) if chain_path.exists() else []
    if chain:
        previous = chain[-1]["chain_sha256"]
    _append_jsonl(chain_path, _chain_row(record, previous))
    verify_grade_ledger(run_dir, ledger)
    return record


def mark_grader_archived(
    run_dir: Path,
    ledger: str,
    run_keys: str | list[str],
    thread_id: str,
    archived_at: datetime,
):
    if archived_at.tzinfo is None or archived_at.utcoffset() is None:
        raise ValueError("archived_at requires a timezone")
    verify_grade_ledger(run_dir, ledger)
    keys = [run_keys] if isinstance(run_keys, str) else list(run_keys)
    recorded = {
        row["run_key"] for row in load_jsonl(run_dir / f"{ledger}.jsonl")
    }
    if not keys or not set(keys) <= recorded:
        raise ValueError("grader archive references an unrecorded grade")
    path = run_dir / "grader-archive.jsonl"
    events = load_jsonl(path) if path.exists() else []
    if any(event.get("thread_id") == thread_id for event in events):
        raise ValueError("duplicate grader archive event")
    event = {
        "ledger": ledger,
        "run_keys": keys,
        "thread_id": thread_id,
        "archived_at": archived_at.astimezone(timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    _append_jsonl(path, event)
    return event


def verify_grades(run_dir: Path):
    expected = set(_grading_items(run_dir))
    ledgers = {}
    for name in ("grades-a", "grades-b"):
        verify_grade_ledger(run_dir, name)
        path = run_dir / f"{name}.jsonl"
        ledgers[name] = load_jsonl(path) if path.exists() else []
        actual = {row["run_key"] for row in ledgers[name]}
        if actual != expected:
            missing = sorted(expected - actual)
            raise ValueError(f"missing grade in {name}: {missing[0] if missing else 'unexpected key'}")
    comparison = compare_grades(ledgers["grades-a"], ledgers["grades-b"])
    audit_path = run_dir / "audit.csv"
    with audit_path.open("w", encoding="utf-8", newline="") as destination:
        writer = csv.DictWriter(destination, fieldnames=("run_key", "reason"))
        writer.writeheader()
        for run_key in comparison["audit_run_keys"]:
            writer.writerow({"run_key": run_key, "reason": "incorrect, unanswerable, or disagreement"})
    return {
        **comparison,
        "human_audit_count": len(comparison["audit_run_keys"]),
        "status": "provisional" if comparison["audit_run_keys"] else "ready",
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser("prepare-grading")
    prepare.add_argument("--results", type=Path, required=True)
    prepare.add_argument("--gold", type=Path, required=True)
    prepare.add_argument("--output", type=Path, required=True)
    prepare.add_argument("--batch-size", type=int, default=25)
    record = commands.add_parser("record-grade")
    record.add_argument("--run-dir", type=Path, required=True)
    record.add_argument("--ledger", choices=("grades-a", "grades-b", "adjudication"), required=True)
    verify = commands.add_parser("verify-grades")
    verify.add_argument("--run-dir", type=Path, required=True)
    archive = commands.add_parser("mark-grader-archived")
    archive.add_argument("--run-dir", type=Path, required=True)
    archive.add_argument("--ledger", choices=("grades-a", "grades-b", "adjudication"), required=True)
    archive.add_argument("--run-key", action="append", required=True)
    archive.add_argument("--thread-id", required=True)
    archive.add_argument("--archived-at", required=True)
    report = commands.add_parser("report")
    report.add_argument("--results", type=Path, required=True)
    report.add_argument("--grades", type=Path, required=True)
    report.add_argument("--seed", type=int, required=True)
    try:
        args = parser.parse_args(argv)
        if args.command == "prepare-grading":
            batches = prepare_grading(
                load_jsonl(args.results), load_jsonl(args.gold), args.output, args.batch_size
            )
            print(json.dumps({"batches": len(batches), "output": str(args.output)}, separators=(",", ":")))
        elif args.command == "record-grade":
            value = json.loads(sys.stdin.read())
            print(_canonical(record_grade(args.run_dir, args.ledger, value)).decode())
        elif args.command == "verify-grades":
            print(_canonical(verify_grades(args.run_dir)).decode())
        elif args.command == "mark-grader-archived":
            print(_canonical(mark_grader_archived(
                args.run_dir, args.ledger, args.run_key, args.thread_id,
                _parse_datetime(args.archived_at),
            )).decode())
        else:
            print(_canonical(aggregate(load_jsonl(args.results), load_jsonl(args.grades), args.seed)).decode())
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
