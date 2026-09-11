import argparse
import hashlib
import json
import math
import os
import platform
import statistics
import subprocess
import sys
import time
from pathlib import Path


class EvaluationFailure(RuntimeError):
    def __init__(self, message, runs=None, warmups=None, context=None):
        super().__init__(message)
        self.runs = runs or []
        self.warmups = warmups or []
        self.context = context
        self.stderr = message


class InvocationFailure(RuntimeError):
    def __init__(self, message, stdout=b"", stderr=b""):
        super().__init__(message)
        self.stdout = stdout
        self.stderr = stderr.decode("utf-8", "replace") if isinstance(stderr, bytes) else str(stderr)


def _positive(value, name):
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ValueError(name)
    return value


def _para(value):
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ValueError("para")
    return value


def _relative_doc(root, raw):
    if not isinstance(raw, str) or not raw or "\\" in raw:
        raise ValueError("unsafe doc")
    path = Path(raw)
    if path.is_absolute() or ":" in raw or ".." in path.parts:
        raise ValueError("unsafe doc")
    resolved = (root / path).resolve()
    try:
        relative = resolved.relative_to(root).as_posix()
    except ValueError as exc:
        raise ValueError("outside doc") from exc
    if relative != raw or not resolved.is_file():
        raise ValueError("invalid doc")
    return relative


def load_cases(lines, root=None):
    root = Path(root or os.getcwd()).resolve()
    cases, ids = [], set()
    for line_number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            case = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError("malformed JSON") from exc
        if not isinstance(case, dict) or not isinstance(case.get("query"), str) or not case["query"].strip():
            raise ValueError("blank query")
        case = dict(case)
        case.setdefault("id", str(line_number))
        if not isinstance(case["id"], str) or not case["id"] or case["id"] in ids:
            raise ValueError("duplicate id")
        ids.add(case["id"])
        for field in ("category", "language"):
            if field in case and not isinstance(case[field], str):
                raise ValueError(field)
        if "required_points" in case and (not isinstance(case["required_points"], list) or any(not isinstance(point, str) for point in case["required_points"])):
            raise ValueError("required_points")
        answerable = case.get("answerable", True)
        if not isinstance(answerable, bool):
            raise ValueError("answerable")
        legacy = "doc" in case or "para" in case
        if legacy:
            if not isinstance(case.get("doc"), str):
                raise ValueError("legacy doc")
            relevant = [{"doc": case["doc"]}]
            if "para" in case:
                relevant[0]["para"] = _para(case["para"])
            if "relevant" in case and case["relevant"] != relevant:
                raise ValueError("contradictory fields")
        else:
            relevant = case.get("relevant", [])
        if not isinstance(relevant, list):
            raise ValueError("relevant")
        if answerable and not relevant:
            raise ValueError("answerable requires target")
        if not answerable and relevant:
            raise ValueError("no-answer has targets")
        normalized, seen = [], set()
        for target in relevant:
            if not isinstance(target, dict) or "doc" not in target:
                raise ValueError("relevant target")
            doc = _relative_doc(root, target["doc"])
            para = _para(target["para"]) if "para" in target else None
            key = (doc, para)
            if key in seen:
                raise ValueError("duplicate target")
            seen.add(key)
            normalized.append({"doc": doc, **({"para": para} if para is not None else {})})
        case["relevant"] = normalized
        cases.append(case)
    if not cases:
        raise ValueError("zero cases")
    return cases


def _validate_hits(raw, root, k):
    if not isinstance(raw, list):
        raise ValueError("invalid hit list")
    hits, seen = [], set()
    for hit in raw:
        if not isinstance(hit, dict) or hit.get("stale"):
            raise ValueError("invalid hit")
        doc = _relative_doc(root, hit.get("doc"))
        if isinstance(hit.get("para"), bool) or not isinstance(hit.get("para"), int) or hit["para"] < 0:
            raise ValueError("invalid hit para")
        if not isinstance(hit.get("snippet"), str):
            raise ValueError("invalid hit snippet")
        key = (doc, hit["para"])
        if key in seen:
            raise ValueError("duplicate hit")
        seen.add(key)
        hits.append({**hit, "doc": doc})
    if len(hits) > k:
        raise ValueError("invalid hit list")
    return hits


def score_case(case, hits):
    targets = {(target["doc"], target.get("para")) for target in case.get("relevant", [])}
    matched, first = set(), 0
    for rank, hit in enumerate(hits, 1):
        for target in targets:
            if hit["doc"] == target[0] and (target[1] is None or hit["para"] == target[1]):
                matched.add(target)
                first = first or rank
    if not case.get("answerable", True):
        return {"nonempty_candidate": bool(hits)}
    return {"evidence_recall": len(matched) / len(targets), "all_evidence_hit": len(matched) == len(targets), "reciprocal_rank": 1 / first if first else 0, "nonempty_candidate": bool(hits)}


def _sha(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _aggregate(runs, cases, include_categories=True):
    by_mode, case_map = {}, {case["id"]: case for case in cases}
    for run in runs:
        by_mode.setdefault(run["mode"], []).append(run)
    result = {}
    for mode, mode_runs in by_mode.items():
        positive = [score_case(case_map[run["id"]], run["hits"]) for run in mode_runs if case_map[run["id"]].get("answerable", True)]
        negative = [score_case(case_map[run["id"]], run["hits"]) for run in mode_runs if not case_map[run["id"]].get("answerable", True)]
        values = lambda name: [item[name] for item in positive]
        latencies = sorted(run["elapsed_ms"] for run in mode_runs)
        result[mode] = {"evidence_recall": statistics.mean(values("evidence_recall")) if positive else None, "all_evidence_hit_rate": statistics.mean(values("all_evidence_hit")) if positive else None, "reciprocal_rank": statistics.mean(values("reciprocal_rank")) if positive else None, "nonempty_candidate_rate": statistics.mean(item["nonempty_candidate"] for item in negative) if negative else None, "latency_median_ms": statistics.median(latencies), "latency_p95_ms": latencies[max(0, math.ceil(len(latencies) * 0.95) - 1)], "mean_hits": statistics.mean(run["hit_count"] for run in mode_runs), "mean_stdout_bytes": statistics.mean(run["stdout_bytes"] for run in mode_runs), "mean_snippet_chars": statistics.mean(run["snippet_chars"] for run in mode_runs)}
    if include_categories:
        for category in sorted({case.get("category") for case in cases if case.get("category") is not None}):
            subset = [case for case in cases if case.get("category") == category]
            ids = {case["id"] for case in subset}
            result.setdefault("categories", {})[category] = _aggregate([run for run in runs if run["id"] in ids], subset, False)
    return result


def _invoke(exe, args, root, timeout, k):
    command = exe + args[:-1] + ["--", args[-1]]
    started = time.perf_counter()
    try:
        process = subprocess.run(command, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=False)
    except subprocess.TimeoutExpired as exc:
        raise InvocationFailure(str(exc), getattr(exc, "output", b""), getattr(exc, "stderr", b"")) from exc
    elapsed_ms = (time.perf_counter() - started) * 1000
    if process.returncode == 2 and not process.stdout:
        raw = []
    elif process.returncode != 0:
        raise InvocationFailure(f"search exit {process.returncode}: {process.stderr.decode('utf-8', 'replace')}", process.stdout, process.stderr)
    else:
        try:
            raw = json.loads(process.stdout.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise InvocationFailure("malformed search output", process.stdout, process.stderr) from exc
    try:
        hits = _validate_hits(raw, root, k)
    except ValueError as exc:
        raise InvocationFailure(str(exc), process.stdout, process.stderr) from exc
    return elapsed_ms, process, hits, command


def run(args, cases):
    root = Path(args.root).resolve()
    exe = [sys.executable, args.ragrep] if args.ragrep.lower().endswith(".py") else [args.ragrep]
    warmups = []
    for mode in args.modes.split(","):
        try:
            elapsed_ms, process, hits, _ = _invoke(exe, ["search", "--json", "--mode", mode, "-k", str(args.k), "--db", args.db, cases[0]["query"]], root, args.timeout, args.k)
        except Exception as exc:
            failure = EvaluationFailure(str(exc), warmups=warmups, context={"mode": mode, "repetition": 0, "case": cases[0]["id"]})
            failure.stderr = getattr(exc, "stderr", "")
            raise failure from exc
        warmups.append({"mode": mode, "elapsed_ms": elapsed_ms, "hit_count": len(hits)})
    runs = []
    for repetition in range(args.repeats):
        modes = args.modes.split(",")
        if repetition % 2:
            modes.reverse()
        for case in cases:
            for mode in modes:
                try:
                    elapsed_ms, process, hits, _ = _invoke(exe, ["search", "--json", "--mode", mode, "-k", str(args.k), "--db", args.db, case["query"]], root, args.timeout, args.k)
                except Exception as exc:
                    failure = EvaluationFailure(str(exc), runs=runs, warmups=warmups, context={"mode": mode, "repetition": repetition + 1, "case": case["id"]})
                    failure.stderr = getattr(exc, "stderr", "")
                    raise failure from exc
                runs.append({"id": case["id"], "mode": mode, "repetition": repetition + 1, "elapsed_ms": elapsed_ms, "stdout_bytes": len(process.stdout), "hit_count": len(hits), "snippet_chars": sum(len(hit["snippet"]) for hit in hits), "hits": hits, **score_case(case, hits)})
    return runs, warmups


def _corpus(root, excluded):
    files = {}
    for path in root.rglob("*"):
        if not path.is_file():
            continue
        relative = path.relative_to(root)
        if any(part.startswith(".") for part in relative.parts) or relative in excluded:
            continue
        if path.suffix.lower() in (".md", ".txt", ".rst"):
            files[relative.as_posix()] = _sha(path)
    if not files:
        raise ValueError("empty corpus")
    return files


def main(argv=None):
    parser = argparse.ArgumentParser()
    for name in ("ragrep", "db", "root", "cases", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--k", type=int, default=5)
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--timeout", type=float, default=60)
    parser.add_argument("--modes", default="text,hybrid")
    output = None
    measurement_started = False
    runs, warmups = [], []
    try:
        args = parser.parse_args(argv)
        output = Path(args.output).resolve()
        if output.exists():
            raise ValueError("output already exists")
        _positive(args.k, "k")
        _positive(args.repeats, "repeats")
        if not math.isfinite(args.timeout) or args.timeout <= 0:
            raise ValueError("timeout")
        modes = args.modes.split(",")
        if not modes or len(set(modes)) != len(modes) or any(mode not in ("text", "hybrid") for mode in modes):
            raise ValueError("modes")
        root = Path(args.root).resolve()
        paths = {name: Path(getattr(args, name)).resolve() for name in ("db", "cases", "ragrep")}
        if not root.is_dir() or any(not path.is_file() for path in paths.values()):
            raise ValueError("missing input")
        args.root, args.db, args.cases, args.ragrep = str(root), str(paths["db"]), str(paths["cases"]), str(paths["ragrep"])
        cases = load_cases(paths["cases"].read_text(encoding="utf-8").splitlines(), root)
        excluded = {path.relative_to(root) for path in (paths["cases"], output) if path.is_relative_to(root)}
        cases_hash = _sha(paths["cases"])
        executable_hash = _sha(paths["ragrep"])
        corpus_before = _corpus(root, excluded)
        measurement_started = True
        runs, warmups = run(args, cases)
        if _sha(paths["cases"]) != cases_hash or _sha(paths["ragrep"]) != executable_hash:
            raise RuntimeError("cases or executable changed during measurement")
        corpus_after = _corpus(root, excluded)
        if corpus_before != corpus_after:
            raise RuntimeError("corpus changed during measurement")
        report = {"timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "settings": vars(args), "cases_sha256": cases_hash, "executable_sha256": executable_hash, "corpus_sha256": corpus_before, "python": sys.version, "platform": platform.platform(), "warmups": warmups, "runs": runs, "aggregate": _aggregate(runs, cases)}
        with output.open("x", encoding="utf-8") as stream:
            json.dump(report, stream, ensure_ascii=False, indent=2)
        return 0
    except Exception as exc:
        if output is not None and measurement_started:
            diagnostic = {"error": str(exc), "stderr": getattr(exc, "stderr", ""), "timestamp": time.time(), "completed_runs": runs, "warmups": warmups}
            if isinstance(exc, EvaluationFailure):
                diagnostic.update({"context": exc.context, "completed_runs": exc.runs, "warmups": exc.warmups})
            try:
                sidecar = output.with_name(output.name + ".error.json")
                index = 1
                while sidecar.exists():
                    sidecar = output.with_name(output.name + f".{index}.error.json")
                    index += 1
                with sidecar.open("x", encoding="utf-8") as stream:
                    json.dump(diagnostic, stream, ensure_ascii=False, indent=2)
            except Exception as write_exc:
                print(f"error: {exc}; diagnostic write failed: {write_exc}", file=sys.stderr)
                return 1
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
