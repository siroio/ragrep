import argparse
from collections import Counter
from html import unescape
import json
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import sys


DEFAULT_TYPE_COUNTS = {
    "direct": 30,
    "paraphrase": 20,
    "distinction": 15,
    "condition": 15,
    "multi": 10,
    "unanswerable": 10,
}
DEFAULT_SPLIT_COUNTS = (30, 70)
DEFAULT_SPLIT_TYPE_COUNTS = (
    {"direct": 9, "paraphrase": 6, "distinction": 5, "condition": 4, "multi": 3, "unanswerable": 3},
    {"direct": 21, "paraphrase": 14, "distinction": 10, "condition": 11, "multi": 7, "unanswerable": 7},
)
DEFAULT_DOMAIN_COUNTS = {
    "rendering": 12,
    "physics": 11,
    "audio": 11,
    "animation": 11,
    "ui": 11,
    "asset_import": 11,
    "build_platform": 11,
    "editor_workflow": 11,
    "scripting_concepts": 11,
}
DEFAULT_SPLIT_DOMAIN_COUNTS = (
    {"rendering": 4, "physics": 3, "audio": 3, "animation": 3, "ui": 3, "asset_import": 3, "build_platform": 4, "editor_workflow": 3, "scripting_concepts": 4},
    {"rendering": 8, "physics": 8, "audio": 8, "animation": 8, "ui": 8, "asset_import": 8, "build_platform": 7, "editor_workflow": 8, "scripting_concepts": 7},
)
DEFAULT_UNANSWERABLE_KIND_COUNTS = {"corpus_outside": 5, "false_premise": 5}
DEFAULT_SPLIT_UNANSWERABLE_KIND_COUNTS = (
    {"corpus_outside": 2, "false_premise": 1},
    {"corpus_outside": 3, "false_premise": 4},
)
REQUIRED_KEYS = {"id", "query", "answer", "required_points", "evidence", "type", "answerable", "domain", "family", "unanswerable_kind"}
HEADING_RE = re.compile(r"^ {0,3}#{1,6}\s+(.+?)\s*#*\s*$")
FAMILY_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")


def load_jsonl(path: Path) -> list[dict]:
    cases = []
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        if not line.strip():
            raise ValueError(f"{path}:{number}: blank line")
        try:
            value = json.loads(line)
        except json.JSONDecodeError as error:
            raise ValueError(f"{path}:{number}: invalid JSON: {error.msg}") from error
        if not isinstance(value, dict):
            raise ValueError(f"{path}:{number}: expected JSON object")
        cases.append(value)
    return cases


def validate_answerability(case: dict) -> None:
    if case["answerable"]:
        if not case["answer"].strip() or not case["required_points"] or not case["evidence"]:
            raise ValueError(f'{case["id"]}: answerable case requires answer, required_points, and evidence')
    elif case["answer"] != "" or case["required_points"] != [] or case["evidence"] != []:
        raise ValueError(f'{case["id"]}: unanswerable case requires empty answer, required_points, and evidence')


def _normalized_heading(value: str) -> str:
    value = re.sub(r"\[([^]]+)\]\([^)]+\)", r"\1", value)
    value = re.sub(r"<[^>]+>", "", value)
    value = re.sub(r"(?<!\w)(\*{1,3}|_{1,3})(?=\S)(.*?)(?<=\S)\1(?!\w)", r"\2", value)
    value = re.sub(r"[`~]", "", value)
    return " ".join(unescape(value).split())


def _has_heading(document: Path, expected: str) -> bool:
    for line in document.read_text(encoding="utf-8").splitlines():
        match = HEADING_RE.match(line)
        if match and _normalized_heading(match.group(1)) == _normalized_heading(expected):
            return True
    return False


def _validate_evidence(case: dict, corpus_root: Path) -> None:
    for evidence in case["evidence"]:
        if not isinstance(evidence, dict) or set(evidence) - {"doc", "heading", "para"} or {"doc", "heading"} - set(evidence):
            raise ValueError(f'{case["id"]}: evidence must contain exactly doc, heading, and optional para')
        if case["answerable"] and "para" not in evidence:
            raise ValueError(f'{case["id"]}: answerable evidence requires para')
        doc_name = evidence["doc"]
        heading = evidence["heading"]
        if not isinstance(doc_name, str) or not doc_name.strip() or not isinstance(heading, str) or not heading.strip():
            raise ValueError(f'{case["id"]}: evidence doc and heading must be non-empty strings')
        relative = Path(doc_name)
        windows_path = PureWindowsPath(doc_name)
        if relative.is_absolute() or windows_path.is_absolute() or ".." in relative.parts or relative.suffix.lower() != ".md":
            raise ValueError(f'{case["id"]}: invalid evidence document {doc_name}')
        root = corpus_root.resolve()
        document = (root / relative).resolve()
        if not document.is_relative_to(root):
            raise ValueError(f'{case["id"]}: invalid evidence document {doc_name}')
        if not document.is_file():
            raise ValueError(f'{case["id"]}: missing evidence document {doc_name}')
        if not _has_heading(document, heading):
            raise ValueError(f'{case["id"]}: missing heading {heading} in {doc_name}')
        if "para" in evidence and (not isinstance(evidence["para"], int) or isinstance(evidence["para"], bool) or evidence["para"] < 0):
            raise ValueError(f'{case["id"]}: para must be an integer greater than or equal to zero')


def _validate_case(case: dict, corpus_root: Path, known_types: set[str], known_domains: set[str], known_unanswerable_kinds: set[str]) -> None:
    if not isinstance(case, dict) or set(case) != REQUIRED_KEYS:
        missing = REQUIRED_KEYS - set(case) if isinstance(case, dict) else set()
        suffix = f"; missing={','.join(sorted(missing))}" if missing else ""
        raise ValueError(f"case must contain exactly the required keys{suffix}")
    for key in ("id", "query", "type"):
        if not isinstance(case[key], str) or not case[key].strip():
            raise ValueError(f"{key} must be a non-empty string")
    identifier = case["id"]
    if not isinstance(case["answer"], str):
        raise ValueError(f"{identifier}: answer must be a string")
    if not isinstance(case["answerable"], bool):
        raise ValueError(f"{identifier}: answerable must be a boolean")
    if not isinstance(case["required_points"], list) or any(not isinstance(point, str) or not point.strip() for point in case["required_points"]):
        raise ValueError(f"{identifier}: required_points must be non-empty strings")
    if not isinstance(case["evidence"], list):
        raise ValueError(f"{identifier}: evidence must be a list")
    if case["type"] not in known_types:
        raise ValueError(f"{identifier}: unknown type {case['type']}")
    if case["domain"] not in known_domains:
        raise ValueError(f'{identifier}: unknown domain {case["domain"]}')
    if not isinstance(case["family"], str) or not FAMILY_RE.fullmatch(case["family"]):
        raise ValueError(f"{identifier}: invalid family")
    if case["answerable"] and case["unanswerable_kind"] is not None:
        raise ValueError(f"{identifier}: answerable case requires null unanswerable_kind")
    if not case["answerable"] and case["unanswerable_kind"] not in known_unanswerable_kinds:
        raise ValueError(f"{identifier}: unanswerable case requires a known unanswerable_kind")
    validate_answerability(case)
    _validate_evidence(case, corpus_root)


def _validate_counts(label: str, actual: Counter, expected: dict[str, int]) -> None:
    if actual != Counter(expected):
        raise ValueError(f"{label} mismatch: actual={dict(sorted(actual.items()))} expected={dict(sorted(expected.items()))}")


def validate_case_sets(development: list[dict], holdout: list[dict], corpus_root: Path,
                       expected_split_counts: tuple[int, int] = DEFAULT_SPLIT_COUNTS,
                       expected_type_counts: dict[str, int] = DEFAULT_TYPE_COUNTS,
                       expected_split_type_counts: tuple[dict[str, int], dict[str, int]] = DEFAULT_SPLIT_TYPE_COUNTS,
                       expected_domain_counts: dict[str, int] = DEFAULT_DOMAIN_COUNTS,
                       expected_split_domain_counts: tuple[dict[str, int], dict[str, int]] = DEFAULT_SPLIT_DOMAIN_COUNTS,
                       expected_unanswerable_kind_counts: dict[str, int] = DEFAULT_UNANSWERABLE_KIND_COUNTS,
                       expected_split_unanswerable_kind_counts: tuple[dict[str, int], dict[str, int]] = DEFAULT_SPLIT_UNANSWERABLE_KIND_COUNTS) -> None:
    all_cases = development + holdout
    for case in all_cases:
        _validate_case(case, corpus_root, set(expected_type_counts), set(expected_domain_counts), set(expected_unanswerable_kind_counts))
    if (len(development), len(holdout)) != expected_split_counts:
        raise ValueError(f"split counts mismatch: development={len(development)} holdout={len(holdout)} expected={expected_split_counts}")
    ids = [case["id"] for case in all_cases]
    duplicates = sorted(identifier for identifier, count in Counter(ids).items() if count > 1)
    if duplicates:
        raise ValueError(f"duplicate case ID: {duplicates[0]}")
    families = [case["family"] for case in all_cases]
    duplicate_families = sorted(family for family, count in Counter(families).items() if count > 1)
    if duplicate_families:
        raise ValueError(f"duplicate family: {duplicate_families[0]}")
    _validate_counts("type counts", Counter(case["type"] for case in all_cases), expected_type_counts)
    _validate_counts("split type counts (development)", Counter(case["type"] for case in development), expected_split_type_counts[0])
    _validate_counts("split type counts (holdout)", Counter(case["type"] for case in holdout), expected_split_type_counts[1])
    _validate_counts("domain counts", Counter(case["domain"] for case in all_cases), expected_domain_counts)
    _validate_counts("split domain counts (development)", Counter(case["domain"] for case in development), expected_split_domain_counts[0])
    _validate_counts("split domain counts (holdout)", Counter(case["domain"] for case in holdout), expected_split_domain_counts[1])
    _validate_counts("unanswerable kind counts", Counter(case["unanswerable_kind"] for case in all_cases if case["unanswerable_kind"] is not None), expected_unanswerable_kind_counts)
    _validate_counts("split unanswerable kind counts (development)", Counter(case["unanswerable_kind"] for case in development if case["unanswerable_kind"] is not None), expected_split_unanswerable_kind_counts[0])
    _validate_counts("split unanswerable kind counts (holdout)", Counter(case["unanswerable_kind"] for case in holdout if case["unanswerable_kind"] is not None), expected_split_unanswerable_kind_counts[1])


def prefixed_doc(logical_doc: str, doc_prefix: str) -> str:
    for value, label in ((logical_doc, "logical document"), (doc_prefix, "document prefix")):
        path = PurePosixPath(value)
        windows_path = PureWindowsPath(value)
        if (not isinstance(value, str) or "\\" in value or path.is_absolute()
                or windows_path.is_absolute() or windows_path.drive or ".." in path.parts):
            raise ValueError(f"invalid {label}: {value}")
    logical_path = PurePosixPath(logical_doc)
    prefix_path = PurePosixPath(doc_prefix)
    return str(prefix_path / logical_path) if doc_prefix else str(logical_path)


def write_holdout_questions(cases: list[dict], output: Path) -> int:
    ids = [case["id"] for case in cases]
    duplicates = sorted(identifier for identifier, count in Counter(ids).items() if count > 1)
    if duplicates:
        raise ValueError(f"duplicate holdout ID: {duplicates[0]}")
    lines = [json.dumps({"id": case["id"], "query": case["query"]}, ensure_ascii=False, separators=(",", ":")) for case in cases]
    output.write_text("\n".join(lines) + ("\n" if lines else ""), encoding="utf-8")
    return len(lines)


def write_retrieval_cases(cases: list[dict], output: Path, doc_prefix: str = "") -> int:
    lines = []
    for case in cases:
        if not case["answerable"]:
            continue
        for evidence in case["evidence"]:
            retrieval = {"query": case["query"], "doc": prefixed_doc(evidence["doc"], doc_prefix)}
            if "para" in evidence:
                retrieval["para"] = evidence["para"]
            lines.append(json.dumps(retrieval, ensure_ascii=False, separators=(",", ":")))
    output.write_text("\n".join(lines) + ("\n" if lines else ""), encoding="utf-8")
    return len(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", type=Path, required=True)
    parser.add_argument("--development", type=Path, required=True)
    parser.add_argument("--holdout", type=Path, required=True)
    parser.add_argument("--export", type=Path, required=True)
    try:
        args = parser.parse_args(argv)
        development = load_jsonl(args.development)
        holdout = load_jsonl(args.holdout)
        validate_case_sets(development, holdout, args.corpus)
        retrieval = write_retrieval_cases(development + holdout, args.export)
    except (OSError, ValueError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    print(f"development={len(development)} holdout={len(holdout)} retrieval={retrieval}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
