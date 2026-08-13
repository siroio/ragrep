import argparse
from collections import Counter
import csv
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path, PurePosixPath, PureWindowsPath
import random
import shutil
import statistics
import sys
import tempfile

from measure_cases import (
    _append_jsonl, _canonical, _chain_row, _parse_datetime, _utc,
    GENESIS_SHA256, TOKEN_KEYS,
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


def _rows_sha256(rows: list[dict]) -> str:
    return hashlib.sha256(b"".join(_canonical(row) + b"\n" for row in rows)).hexdigest()


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
    if "\\" in value:
        return False
    windows = PureWindowsPath(value)
    return (
        not PurePosixPath(value).is_absolute()
        and not windows.is_absolute()
        and not windows.drive
        and not windows.root
        and ".." not in PurePosixPath(value).parts
        and ".." not in windows.parts
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


def aggregate(
    results: list[dict],
    grades: list[dict],
    grading_items: dict[str, dict],
    seed: int,
) -> dict:
    by_grade = {grade.get("run_key"): grade for grade in grades}
    result_keys = {row.get("run_key") for row in results}
    if (
        len(by_grade) != len(grades)
        or result_keys != set(by_grade)
        or result_keys != set(grading_items)
    ):
        raise ValueError("every result requires exactly one final grade")
    analytic = []
    correct = {}
    for result in results:
        run_key = result["run_key"]
        item = grading_items[run_key]
        if item.get("run_key") != run_key or item.get("result", {}).get("run_key") != run_key:
            raise ValueError("grading input does not match result")
        if item["result"].get("id") != result.get("id"):
            raise ValueError("grading input question does not match result")
        gold = item["gold"]
        validate_grade(
            by_grade[run_key], run_key,
            expected_points=list(gold["required_points"]),
            answerable=gold["answerable"],
        )
        correct[run_key] = by_grade[run_key]["correct"]
        analytic.append({
            **result,
            "split": gold["split"],
            "type": gold["type"],
            "domain": gold["domain"],
            "gold_answerable": gold["answerable"],
            "gold_evidence": gold["evidence"],
        })
    by_condition = _breakdown(analytic, correct, "condition")
    for condition, data in by_condition.items():
        rows = [row for row in analytic if row["condition"] == condition]
        data["duration_seconds"] = _stats(row.get("duration_seconds") for row in rows)
        data["tokens"] = {
            key: _stats(row.get("tokens", {}).get(key) for row in rows)
            for key in TOKEN_KEYS
        }
    c0_accuracy = by_condition.get("c0_no_skill_no_ragrep", {}).get("accuracy")
    deltas = {
        condition: (data["accuracy"] - c0_accuracy if c0_accuracy is not None else None)
        for condition, data in by_condition.items()
        if condition != "c0_no_skill_no_ragrep"
    }
    indexed = {(row["condition"], row["repetition"], row["id"]): row for row in analytic}
    regressions = 0
    for row in analytic:
        if row["condition"] != "c0_no_skill_no_ragrep" or not correct[row["run_key"]]:
            continue
        for condition in ("c1_ragrep_no_skill", "c2_skill_ragrep"):
            peer = indexed.get((condition, row["repetition"], row["id"]))
            if peer is not None and not correct[peer["run_key"]]:
                regressions += 1
    retrieval_rows = [
        row for row in analytic
        if row["condition"] != "c0_no_skill_no_ragrep"
        and row["gold_answerable"] and row["gold_evidence"]
    ]
    acquired = {}
    recall = {5: {}, 10: {}}
    for row in retrieval_rows:
        gold = {(item["doc"], item["para"]) for item in row["gold_evidence"]}
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
    unanswerable = [row for row in analytic if not row["gold_answerable"]]
    adopted_evidence = []
    for row in retrieval_rows:
        evidence = row.get("worker_output", {}).get("evidence", [])
        if not evidence:
            continue
        gold = {(item["doc"], item["para"]) for item in row["gold_evidence"]}
        adopted_evidence.append(not any((item["doc"], item["para"]) in gold for item in evidence))
    per_question = {}
    for identifier in sorted({row["id"] for row in analytic}):
        rows = [row for row in analytic if row["id"] == identifier]
        per_question[identifier] = {
            "runs": len(rows),
            "tool_calls": sum(sum(row.get("tool_counts", {}).values()) for row in rows),
            "ragrep_searches": sum(row.get("ragrep_search_count", 0) for row in rows),
            "worker_searches": sum(len(row.get("worker_output", {}).get("searches", [])) for row in rows),
            "retrievals": sum(len(row.get("worker_output", {}).get("retrievals", [])) for row in rows),
            "retrieved_hits": sum(
                len(retrieval.get("hits", []))
                for row in rows
                for retrieval in row.get("worker_output", {}).get("retrievals", [])
            ),
            "rg_calls": sum(row.get("rg_count", 0) for row in rows),
            "file_reads": sum(row.get("file_read_count", 0) for row in rows),
        }
    return {
        "status": "provisional",
        "overall_accuracy": _rate(analytic, correct),
        "holdout_accuracy": _rate([row for row in analytic if row["split"] == "holdout"], correct),
        "unanswerable_rejection_rate": (
            sum(bool(by_grade[row["run_key"]]["unanswerable_correct"]) for row in unanswerable) / len(unanswerable)
            if unanswerable else None
        ),
        "by_condition": by_condition,
        "by_repetition": _breakdown(
            [{**row, "repetition_label": str(row["repetition"])} for row in analytic],
            correct, "repetition_label",
        ),
        "condition_delta_vs_c0": deltas,
        "c0_correct_retrieval_incorrect_count": regressions,
        "by_split": _breakdown(analytic, correct, "split"),
        "by_type": _breakdown(analytic, correct, "type"),
        "by_domain": _breakdown(analytic, correct, "domain"),
        "failure_class_counts": dict(sorted(Counter(
            grade["failure_class"] for grade in grades if grade["failure_class"] is not None
        ).items())),
        "retrieval": {
            "recall_at_5": statistics.mean(recall[5].values()) if recall[5] else None,
            "recall_at_10": statistics.mean(recall[10].values()) if recall[10] else None,
            "evidence_acquired_rate": statistics.mean(acquired.values()) if acquired else None,
            "accuracy_given_evidence": _rate(acquired_rows, correct),
            "wrong_evidence_adoption_rate": statistics.mean(adopted_evidence) if adopted_evidence else None,
        },
        "retrieval_by_condition": retrieval_by_condition,
        "per_question": per_question,
        "duration_seconds": _stats(row.get("duration_seconds") for row in analytic),
        "tokens": {
            key: _stats(row.get("tokens", {}).get(key) for row in analytic) for key in TOKEN_KEYS
        },
        "accuracy_bootstrap_95": _bootstrap_accuracy(analytic, correct, seed),
    }


def prepare_grading(results: list[dict], gold: list[dict], output: Path, batch_size: int = 25):
    if output.exists():
        raise ValueError(f"output already exists: {output}")
    batches = make_grading_batches(results, gold, batch_size)
    output.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    try:
        input_path = stage / "grading-input.jsonl"
        input_path.write_text(
            "".join(_canonical(batch).decode() + "\n" for batch in batches),
            encoding="utf-8",
        )
        manifest = {
            "schema_version": 1,
            "grading_input_sha256": hashlib.sha256(input_path.read_bytes()).hexdigest(),
            "results_sha256": _rows_sha256(results),
            "gold_sha256": _rows_sha256(gold),
            "batch_count": len(batches),
            "run_count": sum(len(batch["items"]) for batch in batches),
        }
        (stage / "grading-input.manifest.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        stage.rename(output)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    return batches


def _grading_items(run_dir: Path):
    input_path = run_dir / "grading-input.jsonl"
    manifest_path = run_dir / "grading-input.manifest.json"
    if not manifest_path.exists():
        raise ValueError("grading input manifest is missing")
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if hashlib.sha256(input_path.read_bytes()).hexdigest() != manifest.get("grading_input_sha256"):
        raise ValueError("grading input hash mismatch")
    items = {}
    batches = load_jsonl(input_path)
    if manifest.get("batch_count") != len(batches):
        raise ValueError("grading input batch count mismatch")
    for expected_batch, batch in enumerate(batches, start=1):
        if batch.get("batch") != expected_batch or not isinstance(batch.get("items"), list):
            raise ValueError("grading input batch is invalid")
        for item in batch.get("items", []):
            run_key = item.get("run_key")
            if not isinstance(run_key, str) or run_key in items:
                raise ValueError("grading input has duplicate or invalid run keys")
            items[run_key] = item
    if manifest.get("run_count") != len(items):
        raise ValueError("grading input run count mismatch")
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
        if not isinstance(record, dict) or not isinstance(record.get("run_key"), str):
            raise ValueError("invalid grade record")
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


def _batch_keys(run_dir: Path, ledger: str) -> dict[int, list[str]]:
    name = "grading-input.jsonl" if ledger in {"grades-a", "grades-b"} else "adjudication-input.jsonl"
    path = run_dir / name
    if not path.exists():
        raise ValueError(f"{name} is missing")
    batches = {}
    for row in load_jsonl(path):
        batch = row.get("batch")
        items = row.get("items")
        if not isinstance(batch, int) or batch in batches or not isinstance(items, list):
            raise ValueError("invalid grader batch")
        keys = [item.get("run_key") for item in items]
        if any(not isinstance(key, str) for key in keys) or len(keys) != len(set(keys)):
            raise ValueError("invalid grader batch run keys")
        batches[batch] = keys
    return batches


def _assignments(run_dir: Path) -> list[dict]:
    rows = load_jsonl(run_dir / "grader-assignments.jsonl") if (run_dir / "grader-assignments.jsonl").exists() else []
    seen_batches = set()
    seen_threads = set()
    covered = {name: set() for name in ("grades-a", "grades-b", "adjudication")}
    for row in rows:
        if set(row) != {"ledger", "batch", "run_keys", "thread_id", "assigned_at"}:
            raise ValueError("grader assignment has invalid fields")
        ledger = row["ledger"]
        if ledger not in covered or (ledger, row["batch"]) in seen_batches:
            raise ValueError("duplicate grader batch assignment")
        if not isinstance(row["thread_id"], str) or not row["thread_id"] or row["thread_id"] in seen_threads:
            raise ValueError("duplicate grader thread assignment")
        if not isinstance(row["run_keys"], list) or len(row["run_keys"]) != len(set(row["run_keys"])):
            raise ValueError("grader assignment has invalid run keys")
        if covered[ledger] & set(row["run_keys"]):
            raise ValueError("overlapping grader assignment")
        seen_batches.add((ledger, row["batch"]))
        seen_threads.add(row["thread_id"])
        covered[ledger].update(row["run_keys"])
    return rows


def assign_grader(
    run_dir: Path,
    ledger: str,
    batch: int,
    thread_id: str,
    assigned_at: datetime,
) -> dict:
    if ledger not in {"grades-a", "grades-b", "adjudication"}:
        raise ValueError("invalid grade ledger")
    batches = _batch_keys(run_dir, ledger)
    if batch not in batches:
        raise ValueError("unknown grader batch")
    assignments = _assignments(run_dir)
    if any(row["ledger"] == ledger and row["batch"] == batch for row in assignments):
        raise ValueError("duplicate grader batch assignment")
    if any(row["thread_id"] == thread_id for row in assignments):
        raise ValueError("duplicate grader thread assignment")
    event = {
        "ledger": ledger,
        "batch": batch,
        "run_keys": batches[batch],
        "thread_id": thread_id,
        "assigned_at": _utc(assigned_at, "assigned_at"),
    }
    _append_jsonl(run_dir / "grader-assignments.jsonl", event)
    return event


def _assignment_for(run_dir: Path, ledger: str, run_key: str, thread_id: str) -> dict:
    matches = [
        row for row in _assignments(run_dir)
        if row["ledger"] == ledger and row["thread_id"] == thread_id and run_key in row["run_keys"]
    ]
    if len(matches) != 1:
        raise ValueError("grade does not match an assigned grader batch")
    return matches[0]


def record_grade(
    run_dir: Path, ledger: str, record: dict, *, thread_id: str
):
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
    _assignment_for(run_dir, ledger, run_key, thread_id)
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
    thread_id: str,
    archived_at: datetime,
):
    verify_grade_ledger(run_dir, ledger)
    assignments = [
        row for row in _assignments(run_dir)
        if row["ledger"] == ledger and row["thread_id"] == thread_id
    ]
    if len(assignments) != 1:
        raise ValueError("grader archive does not match an assignment")
    keys = assignments[0]["run_keys"]
    recorded = {
        row["run_key"] for row in load_jsonl(run_dir / f"{ledger}.jsonl")
    }
    if not keys or not set(keys) <= recorded:
        raise ValueError("grader archive references an incomplete batch")
    path = run_dir / "grader-archive.jsonl"
    events = load_jsonl(path) if path.exists() else []
    if any(event.get("thread_id") == thread_id for event in events):
        raise ValueError("duplicate grader archive event")
    event = {
        "ledger": ledger,
        "run_keys": keys,
        "thread_id": thread_id,
        "archived_at": _utc(archived_at, "archived_at"),
    }
    _append_jsonl(path, event)
    return event


def prepare_adjudication(run_dir: Path, batch_size: int = 25) -> list[dict]:
    if batch_size < 1:
        raise ValueError("batch_size must be positive")
    path = run_dir / "adjudication-input.jsonl"
    if path.exists():
        raise ValueError("adjudication input already exists")
    items = _grading_items(run_dir)
    verify_grade_ledger(run_dir, "grades-a")
    verify_grade_ledger(run_dir, "grades-b")
    a = load_jsonl(run_dir / "grades-a.jsonl")
    b = load_jsonl(run_dir / "grades-b.jsonl")
    if {row.get("run_key") for row in a} != set(items) or {row.get("run_key") for row in b} != set(items):
        raise ValueError("complete A/B grades are required before adjudication")
    for records in (a, b):
        for record in records:
            gold = items[record["run_key"]]["gold"]
            validate_grade(
                record, record["run_key"],
                expected_points=list(gold["required_points"]),
                answerable=gold["answerable"],
            )
    comparison = compare_grades(a, b)
    by_a = {row["run_key"]: row for row in a}
    by_b = {row["run_key"]: row for row in b}
    rows = [
        {"run_key": run_key, "item": items[run_key], "grade_a": by_a[run_key], "grade_b": by_b[run_key]}
        for run_key in comparison["needs_adjudication"]
    ]
    batches = [
        {"batch": index // batch_size + 1, "items": rows[index:index + batch_size]}
        for index in range(0, len(rows), batch_size)
    ]
    path.write_text("".join(_canonical(row).decode() + "\n" for row in batches), encoding="utf-8")
    return batches


def _write_resolved(run_dir: Path, records: list[dict]) -> None:
    path = run_dir / "resolved-grades.jsonl"
    chain_path = run_dir / "resolved-grades.sha256-chain.jsonl"
    rendered = "".join(_canonical(row).decode() + "\n" for row in records)
    previous = GENESIS_SHA256
    chains = []
    for record in records:
        chain = _chain_row(record, previous)
        chains.append(chain)
        previous = chain["chain_sha256"]
    chain_rendered = "".join(_canonical(row).decode() + "\n" for row in chains)
    if path.exists() or chain_path.exists():
        if not path.exists() or not chain_path.exists() or path.read_text(encoding="utf-8") != rendered or chain_path.read_text(encoding="utf-8") != chain_rendered:
            raise ValueError("resolved grade artifact conflicts with verified grades")
        return
    stage = Path(tempfile.mkdtemp(prefix=".resolved-grades.", dir=run_dir))
    try:
        for name, value in (
            ("resolved-grades.jsonl", rendered),
            ("resolved-grades.sha256-chain.jsonl", chain_rendered),
        ):
            with (stage / name).open("w", encoding="utf-8", newline="\n") as destination:
                destination.write(value)
                destination.flush()
                os.fsync(destination.fileno())
        (stage / "resolved-grades.jsonl").replace(path)
        (stage / "resolved-grades.sha256-chain.jsonl").replace(chain_path)
    finally:
        if stage.exists():
            shutil.rmtree(stage)


def _verify_grader_archives(run_dir: Path, expected_keys: dict[str, set[str]]) -> None:
    assignments = _assignments(run_dir)
    covered = {ledger: set() for ledger in expected_keys}
    for assignment in assignments:
        batches = _batch_keys(run_dir, assignment["ledger"])
        if batches.get(assignment["batch"]) != assignment["run_keys"]:
            raise ValueError("grader assignment does not match its exact batch")
        covered[assignment["ledger"]].update(assignment["run_keys"])
    for ledger, expected in expected_keys.items():
        if covered[ledger] != expected:
            raise ValueError(f"missing grader assignment in {ledger}")
    archives = load_jsonl(run_dir / "grader-archive.jsonl") if (run_dir / "grader-archive.jsonl").exists() else []
    by_thread = {row["thread_id"]: row for row in assignments}
    archived_threads = set()
    for event in archives:
        if set(event) != {"ledger", "run_keys", "thread_id", "archived_at"}:
            raise ValueError("grader archive has invalid fields")
        assignment = by_thread.get(event["thread_id"])
        if not assignment or event["ledger"] != assignment["ledger"] or event["run_keys"] != assignment["run_keys"]:
            raise ValueError("grader archive does not match assignment")
        if event["thread_id"] in archived_threads:
            raise ValueError("duplicate grader archive event")
        archived_threads.add(event["thread_id"])
    missing = sorted(set(by_thread) - archived_threads)
    if missing:
        raise ValueError(f"unarchived grader task: {missing[0]}")


def verify_grades(run_dir: Path):
    items = _grading_items(run_dir)
    expected = set(items)
    ledgers = {}
    for name in ("grades-a", "grades-b"):
        verify_grade_ledger(run_dir, name)
        path = run_dir / f"{name}.jsonl"
        ledgers[name] = load_jsonl(path) if path.exists() else []
        actual = {row["run_key"] for row in ledgers[name]}
        if actual != expected:
            missing = sorted(expected - actual)
            raise ValueError(f"missing grade in {name}: {missing[0] if missing else 'unexpected key'}")
        for record in ledgers[name]:
            gold = items[record["run_key"]]["gold"]
            validate_grade(record, record["run_key"], expected_points=list(gold["required_points"]), answerable=gold["answerable"])
    comparison = compare_grades(ledgers["grades-a"], ledgers["grades-b"])
    verify_grade_ledger(run_dir, "adjudication")
    adjudications = load_jsonl(run_dir / "adjudication.jsonl") if (run_dir / "adjudication.jsonl").exists() else []
    by_adjudication = {row.get("run_key"): row for row in adjudications}
    needed = set(comparison["needs_adjudication"])
    if len(by_adjudication) != len(adjudications):
        raise ValueError("duplicate adjudication")
    if set(by_adjudication) != needed:
        missing = sorted(needed - set(by_adjudication))
        extra = sorted(set(by_adjudication) - needed)
        if missing:
            raise ValueError(f"missing adjudication: {missing[0]}")
        raise ValueError(f"extra adjudication: {extra[0]}")
    for run_key, record in by_adjudication.items():
        gold = items[run_key]["gold"]
        validate_grade(record, run_key, expected_points=list(gold["required_points"]), answerable=gold["answerable"])
    by_a = {row["run_key"]: row for row in ledgers["grades-a"]}
    resolved = [by_adjudication.get(run_key, by_a[run_key]) for run_key in sorted(expected)]
    expected_assignments = {
        "grades-a": expected,
        "grades-b": expected,
        "adjudication": needed,
    }
    _verify_grader_archives(run_dir, expected_assignments)
    _write_resolved(run_dir, resolved)
    audit_path = run_dir / "audit.csv"
    with audit_path.open("w", encoding="utf-8", newline="") as destination:
        writer = csv.DictWriter(destination, fieldnames=("run_key", "reason"))
        writer.writeheader()
        audit_keys = sorted({
            run_key for run_key in expected
            if run_key in needed
            or not next(row for row in resolved if row["run_key"] == run_key)["correct"]
            or items[run_key]["gold"]["answerable"] is False
        })
        for run_key in audit_keys:
            writer.writerow({"run_key": run_key, "reason": "incorrect, unanswerable, or disagreement"})
    return {
        **comparison,
        "resolved_count": len(resolved),
        "human_audit_count": len(audit_keys),
        "status": "provisional" if audit_keys else "ready",
    }


def report(run_dir: Path, results: list[dict], seed: int, output: Path) -> dict:
    if output.exists():
        raise ValueError(f"output already exists: {output}")
    grading_manifest = json.loads(
        (run_dir / "grading-input.manifest.json").read_text(encoding="utf-8")
    )
    if _rows_sha256(results) != grading_manifest.get("results_sha256"):
        raise ValueError("results hash does not match immutable grading input")
    verification = verify_grades(run_dir)
    resolved = load_jsonl(run_dir / "resolved-grades.jsonl")
    report_value = aggregate(results, resolved, _grading_items(run_dir), seed)
    report_value["status"] = verification["status"]
    report_value["human_audit_count"] = verification["human_audit_count"]
    output.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    try:
        (stage / "summary.json").write_text(
            json.dumps(report_value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        with (stage / "by-condition.csv").open("w", encoding="utf-8", newline="") as destination:
            writer = csv.DictWriter(destination, fieldnames=(
                "condition", "count", "accuracy", "duration_mean",
                *[f"{key}_mean" for key in TOKEN_KEYS],
            ))
            writer.writeheader()
            for condition, data in report_value["by_condition"].items():
                writer.writerow({
                    "condition": condition,
                    "count": data["count"],
                    "accuracy": data["accuracy"],
                    "duration_mean": data["duration_seconds"]["mean"],
                    **{f"{key}_mean": data["tokens"][key]["mean"] for key in TOKEN_KEYS},
                })
        with (stage / "by-repetition.csv").open("w", encoding="utf-8", newline="") as destination:
            writer = csv.DictWriter(destination, fieldnames=("repetition", "count", "accuracy"))
            writer.writeheader()
            for repetition, data in report_value["by_repetition"].items():
                writer.writerow({"repetition": repetition, **data})
        question_fields = (
            "id", "runs", "tool_calls", "ragrep_searches", "worker_searches",
            "retrievals", "retrieved_hits", "rg_calls", "file_reads",
        )
        with (stage / "per-question.csv").open("w", encoding="utf-8", newline="") as destination:
            writer = csv.DictWriter(destination, fieldnames=question_fields)
            writer.writeheader()
            for identifier, data in report_value["per_question"].items():
                writer.writerow({"id": identifier, **data})
        stage.rename(output)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    return report_value


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
    record.add_argument("--thread-id", required=True)
    assign = commands.add_parser("assign-grader")
    assign.add_argument("--run-dir", type=Path, required=True)
    assign.add_argument("--ledger", choices=("grades-a", "grades-b", "adjudication"), required=True)
    assign.add_argument("--batch", type=int, required=True)
    assign.add_argument("--thread-id", required=True)
    assign.add_argument("--assigned-at", required=True)
    adjudication = commands.add_parser("prepare-adjudication")
    adjudication.add_argument("--run-dir", type=Path, required=True)
    adjudication.add_argument("--batch-size", type=int, default=25)
    verify = commands.add_parser("verify-grades")
    verify.add_argument("--run-dir", type=Path, required=True)
    archive = commands.add_parser("mark-grader-archived")
    archive.add_argument("--run-dir", type=Path, required=True)
    archive.add_argument("--ledger", choices=("grades-a", "grades-b", "adjudication"), required=True)
    archive.add_argument("--thread-id", required=True)
    archive.add_argument("--archived-at", required=True)
    report_parser = commands.add_parser("report")
    report_parser.add_argument("--results", type=Path, required=True)
    report_parser.add_argument("--run-dir", type=Path, required=True)
    report_parser.add_argument("--output", type=Path, required=True)
    report_parser.add_argument("--seed", type=int, required=True)
    try:
        args = parser.parse_args(argv)
        if args.command == "prepare-grading":
            batches = prepare_grading(
                load_jsonl(args.results), load_jsonl(args.gold), args.output, args.batch_size
            )
            print(json.dumps({"batches": len(batches), "output": str(args.output)}, separators=(",", ":")))
        elif args.command == "record-grade":
            value = json.loads(sys.stdin.read())
            print(_canonical(record_grade(
                args.run_dir, args.ledger, value, thread_id=args.thread_id
            )).decode())
        elif args.command == "assign-grader":
            print(_canonical(assign_grader(
                args.run_dir, args.ledger, args.batch, args.thread_id,
                _parse_datetime(args.assigned_at),
            )).decode())
        elif args.command == "prepare-adjudication":
            batches = prepare_adjudication(args.run_dir, args.batch_size)
            print(_canonical({"batches": len(batches)}).decode())
        elif args.command == "verify-grades":
            print(_canonical(verify_grades(args.run_dir)).decode())
        elif args.command == "mark-grader-archived":
            print(_canonical(mark_grader_archived(
                args.run_dir, args.ledger, args.thread_id,
                _parse_datetime(args.archived_at),
            )).decode())
        else:
            print(_canonical(report(
                args.run_dir, load_jsonl(args.results), args.seed, args.output
            )).decode())
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
