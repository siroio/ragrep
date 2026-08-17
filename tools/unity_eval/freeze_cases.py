import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
from datetime import datetime, timezone
import sys

import validate_cases
from validate_cases import (
    load_jsonl,
    validate_case_sets,
    write_holdout_questions,
    write_retrieval_cases,
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def verify_holdout_questions(gold_cases: list[dict], questions_path: Path) -> None:
    questions = load_jsonl(questions_path)
    expected = [{"id": case["id"], "query": case["query"]} for case in gold_cases]
    if questions == expected:
        return
    for index, case in enumerate(expected):
        if index >= len(questions) or questions[index] != case:
            raise ValueError(f'{case["id"]}: holdout question mismatch')
    extra_id = questions[len(expected)].get("id", "unknown")
    raise ValueError(f"{extra_id}: extra holdout question")


def validate_allocation(
    allocation_path: Path, development: list[dict], holdout: list[dict]
) -> None:
    slots = json.loads(allocation_path.read_text(encoding="utf-8"))
    if not isinstance(slots, list):
        raise ValueError("allocation must be a JSON array")
    expected = {
        case["id"]: {
            "id": case["id"],
            "split": split,
            "type": case["type"],
            "domain": case["domain"],
            "unanswerable_kind": case["unanswerable_kind"],
            "family": case["family"],
            "status": "approved",
        }
        for split, cases in (("development", development), ("holdout", holdout))
        for case in cases
    }
    actual = {}
    for slot in slots:
        if not isinstance(slot, dict) or not isinstance(slot.get("id"), str):
            raise ValueError("allocation slot requires an ID")
        identifier = slot["id"]
        if identifier in actual:
            raise ValueError(f"{identifier}: duplicate allocation slot")
        actual[identifier] = slot
    if set(actual) != set(expected):
        difference = sorted(set(actual) ^ set(expected))
        raise ValueError(f"{difference[0]}: allocation case mismatch")
    for identifier, wanted in expected.items():
        if set(actual[identifier]) != set(wanted):
            raise ValueError(f"{identifier}: allocation fields mismatch")
        for key, value in wanted.items():
            if actual[identifier].get(key) != value:
                raise ValueError(f"{identifier}: allocation {key} mismatch")


def validate_reviews(review_path: Path, cases: list[dict]) -> None:
    latest = {}
    seen_rounds = set()
    for record in load_jsonl(review_path):
        identifier = record.get("id")
        round_number = record.get("round")
        if not isinstance(identifier, str) or not isinstance(round_number, int) or isinstance(round_number, bool):
            raise ValueError("review requires an ID and integer round")
        key = (identifier, round_number)
        if key in seen_rounds:
            raise ValueError(f"{identifier}: duplicate review round {round_number}")
        seen_rounds.add(key)
        if identifier not in latest or round_number > latest[identifier]["round"]:
            latest[identifier] = record
    expected_ids = {case["id"] for case in cases}
    if set(latest) != expected_ids:
        difference = sorted(set(latest) ^ expected_ids)
        raise ValueError(f"{difference[0]}: review case mismatch")
    for identifier, record in latest.items():
        if record.get("verdict") != "approved":
            raise ValueError(f"{identifier}: latest review is not approved")


def freeze_case_set(
    corpus_root: Path,
    corpus_manifest: Path,
    development: Path,
    holdout_gold: Path,
    allocation: Path,
    review: Path,
    public_output: Path,
    private_output: Path,
    doc_prefix: str,
    generated_at: datetime,
    validation_kwargs: dict | None = None,
) -> dict:
    if public_output.exists():
        raise ValueError(f"output already exists: {public_output}")
    if private_output.exists():
        raise ValueError(f"output already exists: {private_output}")
    if generated_at.tzinfo is None or generated_at.utcoffset() is None:
        raise ValueError("generated_at requires a timezone")
    corpus_data = json.loads(corpus_manifest.read_text(encoding="utf-8"))
    unity_version = corpus_data.get("unity_version") if isinstance(corpus_data, dict) else None
    if not isinstance(unity_version, str) or not unity_version.strip():
        raise ValueError("corpus manifest requires a non-empty string unity_version")
    development_cases = load_jsonl(development)
    holdout_cases = load_jsonl(holdout_gold)
    validate_case_sets(
        development_cases,
        holdout_cases,
        corpus_root,
        **(validation_kwargs or {}),
    )
    validate_allocation(allocation, development_cases, holdout_cases)
    validate_reviews(review, development_cases + holdout_cases)
    validate_cases.prefixed_doc("document.md", doc_prefix)
    normalized_generated_at = generated_at.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")

    public_output.parent.mkdir(parents=True, exist_ok=True)
    private_output.parent.mkdir(parents=True, exist_ok=True)
    public_stage = None
    private_stage = None
    private_published = False
    try:
        public_stage = Path(tempfile.mkdtemp(prefix=f".{public_output.name}.", dir=public_output.parent))
        private_stage = Path(tempfile.mkdtemp(prefix=f".{private_output.name}.", dir=private_output.parent))
        shutil.copyfile(development, public_stage / "development.jsonl")
        shutil.copyfile(holdout_gold, private_stage / "holdout-gold.jsonl")
        shutil.copyfile(allocation, private_stage / "allocation.json")
        shutil.copyfile(review, private_stage / "review.jsonl")
        write_holdout_questions(holdout_cases, public_stage / "holdout-questions.jsonl")
        verify_holdout_questions(holdout_cases, public_stage / "holdout-questions.jsonl")
        retrieval_count = write_retrieval_cases(
            development_cases + holdout_cases,
            public_stage / "retrieval.jsonl",
            doc_prefix,
        )
        all_cases = development_cases + holdout_cases
        manifest = {
            "schema_version": 1,
            "unity_version": unity_version,
            "generated_at": normalized_generated_at,
            "counts": {
                "development": len(development_cases),
                "holdout": len(holdout_cases),
                "total": len(all_cases),
                "answerable": sum(1 for case in all_cases if case["answerable"]),
                "retrieval_entries": retrieval_count,
            },
            "doc_prefix": doc_prefix,
            "sha256": {
                "corpus_manifest": sha256_file(corpus_manifest),
                "development": sha256_file(public_stage / "development.jsonl"),
                "holdout_questions": sha256_file(public_stage / "holdout-questions.jsonl"),
                "holdout_gold": sha256_file(private_stage / "holdout-gold.jsonl"),
                "allocation": sha256_file(private_stage / "allocation.json"),
                "review": sha256_file(private_stage / "review.jsonl"),
                "validator": sha256_file(Path(validate_cases.__file__)),
            },
        }
        (public_stage / "manifest.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        private_stage.rename(private_output)
        private_published = True
        public_stage.rename(public_output)
        return manifest
    except Exception:
        if private_published:
            shutil.rmtree(private_output)
        raise
    finally:
        if public_stage is not None and public_stage.exists():
            shutil.rmtree(public_stage)
        if private_stage is not None and private_stage.exists():
            shutil.rmtree(private_stage)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", type=Path, required=True)
    parser.add_argument("--corpus-manifest", type=Path, required=True)
    parser.add_argument("--development", type=Path, required=True)
    parser.add_argument("--holdout-gold", type=Path, required=True)
    parser.add_argument("--allocation", type=Path, required=True)
    parser.add_argument("--review", type=Path, required=True)
    parser.add_argument("--public-output", type=Path, required=True)
    parser.add_argument("--private-output", type=Path, required=True)
    parser.add_argument("--doc-prefix", required=True)
    parser.add_argument("--generated-at", required=True)
    args = parser.parse_args(argv)
    try:
        generated_at = datetime.fromisoformat(args.generated_at)
        if generated_at.tzinfo is None or generated_at.utcoffset() is None:
            raise ValueError("--generated-at requires a timezone")
        manifest = freeze_case_set(
            corpus_root=args.corpus,
            corpus_manifest=args.corpus_manifest,
            development=args.development,
            holdout_gold=args.holdout_gold,
            allocation=args.allocation,
            review=args.review,
            public_output=args.public_output,
            private_output=args.private_output,
            doc_prefix=args.doc_prefix,
            generated_at=generated_at,
        )
    except (OSError, ValueError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    counts = manifest["counts"]
    print(
        f'development={counts["development"]} holdout={counts["holdout"]} '
        f'answerable={counts["answerable"]} retrieval={counts["retrieval_entries"]} '
        f"public={args.public_output} private={args.private_output}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
