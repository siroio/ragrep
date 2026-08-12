import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import random
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
    dispatch = []
    for condition in CONDITIONS:
        for repetition in range(1, repetitions + 1):
            material = f"{seed}:{condition}:{repetition}".encode()
            local_seed = int.from_bytes(hashlib.sha256(material).digest())
            shuffled = list(questions)
            random.Random(local_seed).shuffle(shuffled)
            for order, question in enumerate(shuffled, start=1):
                dispatch.append({
                    "condition": condition,
                    "repetition": repetition,
                    "order": order,
                    "id": question["id"],
                    "query": question["query"],
                    "split": question["split"],
                    "run_key": f'{condition}/r{repetition}/{question["id"]}',
                })
    return dispatch


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


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
    output: Path,
    pilot_ids: tuple[str, str, str],
    seed: int,
    generated_at: datetime,
    prompt_hashes: dict[str, str],
    repetitions: int = 5,
) -> dict:
    if output.exists():
        raise ValueError(f"output already exists: {output}")
    if generated_at.tzinfo is None or generated_at.utcoffset() is None:
        raise ValueError("generated_at requires a timezone")
    if set(prompt_hashes) != set(CONDITIONS) or any(
        not isinstance(value, str) or len(value) != 64 for value in prompt_hashes.values()
    ):
        raise ValueError("prompt_hashes must contain one SHA-256 per condition")
    sources = {
        "frozen_manifest": frozen_manifest,
        "holdout_questions": holdout_questions,
        "development": development,
        "holdout_gold": holdout_gold,
        "validator": validator,
        "skill": skill,
        "db": db,
        "binary": binary,
    }
    fixed_hashes = {name: sha256_file(path) for name, path in sources.items()}
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
        for condition in CONDITIONS
        for order, identifier in enumerate(pilot_ids, start=1)
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
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    try:
        _write_jsonl(stage / "questions-only.jsonl", public_questions)
        _write_jsonl(stage / "dispatch.jsonl", dispatch)
        _write_jsonl(stage / "pilot-dispatch.jsonl", pilot)
        (stage / "manifest.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        stage.rename(output)
        return manifest
    finally:
        if stage.exists():
            shutil.rmtree(stage)


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
        "validator", "skill", "db", "binary", "output",
    ):
        prepare.add_argument(f"--{name}", type=Path, required=True)
    prepare.add_argument("--pilot-id", action="append", required=True)
    prepare.add_argument("--seed", type=int, required=True)
    prepare.add_argument("--generated-at", required=True)
    for condition in CONDITIONS:
        prepare.add_argument(f"--prompt-hash-{condition}", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "prepare":
            if len(args.pilot_id) != 3:
                raise ValueError("exactly three --pilot-id values are required")
            kwargs = vars(args)
            prompt_hashes = {
                condition: kwargs[f"prompt_hash_{condition}"] for condition in CONDITIONS
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
                output=args.output,
                pilot_ids=tuple(args.pilot_id),
                seed=args.seed,
                generated_at=_parse_datetime(args.generated_at),
                prompt_hashes=prompt_hashes,
            )
            print(f'questions={manifest["question_count"]} runs={manifest["run_count"]} output={args.output}')
            return 0
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
