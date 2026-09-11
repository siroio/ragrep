#!/usr/bin/env python3
"""Build and package a host ragrep release with deterministic metadata."""

import argparse
import hashlib
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import zipfile
from pathlib import Path

TARGETS = {"windows-amd64", "windows-arm64", "linux-amd64", "darwin-arm64"}
EPOCH = (1980, 1, 1, 0, 0, 0)
NOTICE_SOURCE = Path(__file__).with_name("notices")
REQUIRED_NOTICE_PATHS = (
    "INVENTORY.txt",
    "gemma/Notice",
    "gemma/Gemma-Terms-of-Use.txt",
    "gemma/Gemma-Prohibited-Use-Policy.txt",
    "sqlite-vec/LICENSE-MIT",
    "sqlite-vec/LICENSE-APACHE",
    "sqlite-vec/SQLITE-PUBLIC-DOMAIN.txt",
)


def run(command, cwd=None):
    return subprocess.run(command, cwd=cwd, check=True, text=True, capture_output=True)


def metadata(args):
    return {
        "version": args.version,
        "revision": args.revision,
        "dirty": args.dirty,
        "go_version": args.go_version,
        "target": args.target,
        "asset_pins": args.asset_pin,
        "built_at": args.built_at,
    }


def required_notice_paths(target):
    required = list(REQUIRED_NOTICE_PATHS)
    if target in {"linux-amd64", "darwin-arm64"}:
        required.extend(
            f"native/{target}/onnxruntime-1.24.4/{name}"
            for name in ("LICENSE", "ThirdPartyNotices.txt", "Privacy.md")
        )
    else:
        required.extend(
            f"native/{target}/{component}/{name}"
            for component, names in (
                ("onnxruntime-directml-1.24.4", ("LICENSE", "ThirdPartyNotices.txt", "Privacy.md")),
                ("directml-1.15.4", ("LICENSE.txt", "LICENSE-CODE.txt", "ThirdPartyNotices.txt")),
            )
            for name in names
        )
    return required


def stage_notice_bundle(source, stage, target):
    source = Path(source).resolve()
    if not source.is_dir():
        raise ValueError(f"notice source directory does not exist: {source}")
    for relative in required_notice_paths(target):
        if not (source / relative).is_file():
            raise ValueError(f"required notice file is missing: {source / relative}")
    destination = Path(stage) / "THIRD_PARTY_NOTICES"
    files = []
    for file in source.rglob("*"):
        if not file.is_file():
            continue
        relative = file.relative_to(source)
        parts = relative.parts
        if len(parts) >= 2 and parts[0] == "native" and parts[1] in TARGETS and parts[1] != target:
            continue
        files.append((relative, file))
    if not files:
        raise ValueError(f"notice source directory is empty: {source}")
    for relative, file in sorted(files, key=lambda item: item[0].as_posix()):
        output = destination / relative
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(file.read_bytes())


def archive(source, output, args):
    source = Path(source).resolve()
    output = Path(output).resolve()
    if not source.is_dir():
        raise ValueError(f"source directory does not exist: {source}")
    if output.exists():
        raise ValueError(f"refusing to overwrite: {output}")
    sidecar = output.with_name(output.name + ".sha256")
    if sidecar.exists():
        raise ValueError(f"refusing to overwrite: {sidecar}")
    files = sorted(p for p in source.rglob("*") if p.is_file())
    if not files:
        raise ValueError("source directory is empty")
    output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(output, "x", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as bundle:
        for file in files:
            name = file.relative_to(source).as_posix()
            info = zipfile.ZipInfo(name, EPOCH)
            info.compress_type = zipfile.ZIP_DEFLATED
            mode = 0o755 if name.rsplit("/", 1)[-1] in {"ragrep", "ragrep.exe"} else 0o644
            info.external_attr = (0o100000 | mode) << 16
            bundle.writestr(info, file.read_bytes())
        info = zipfile.ZipInfo("RELEASE.json", EPOCH)
        info.compress_type = zipfile.ZIP_DEFLATED
        info.external_attr = 0o100644 << 16
        bundle.writestr(info, json.dumps(metadata(args), sort_keys=True, indent=2).encode() + b"\n")
    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    with sidecar.open("x", encoding="ascii") as checksum:
        checksum.write(f"{digest}  {output.name}\n")


def build(args):
    root = Path(args.source).resolve()
    if args.target not in TARGETS:
        raise ValueError(f"unsupported target: {args.target}")
    host = ("windows" if os.name == "nt" else "darwin" if sys.platform == "darwin" else "linux") + "-" + ("amd64" if platform.machine().lower() in {"amd64", "x86_64"} else "arm64" if platform.machine().lower() in {"arm64", "aarch64"} else platform.machine().lower())
    if args.target != host:
        raise ValueError(f"host build only: target {args.target}, host {host}")
    go = os.environ.get("GO", "go")
    go_version = run([go, "version"], cwd=root).stdout.strip()
    revision = args.revision or run(["git", "rev-parse", "HEAD"], cwd=root).stdout.strip()
    dirty = args.dirty or bool(run(["git", "status", "--porcelain"], cwd=root).stdout.strip())
    args.revision, args.dirty, args.go_version = revision, dirty, go_version
    with tempfile.TemporaryDirectory(prefix="ragrep-release-") as temp:
        stage = Path(temp) / "package"
        stage.mkdir()
        binary = stage / ("ragrep.exe" if args.target.startswith("windows-") else "ragrep")
        env = os.environ.copy()
        env.update({"CGO_ENABLED": "1", "GOOS": args.target.split("-")[0], "GOARCH": args.target.split("-")[1]})
        command = [go, "-C", "src", "build", "-trimpath", "-ldflags", f"-s -w -X main.version={args.version} -X main.commit={revision} -X main.buildDate={args.built_at}", "-o", str(binary), "./cmd/ragrep"]
        subprocess.run(command, cwd=root, env=env, check=True)
        shutil.copy2(root / "LICENSE", stage / "LICENSE")
        (stage / "INSTALL.md").write_text("Install: put ragrep on PATH, then run `ragrep init` in a workspace.\n", encoding="utf-8")
        stage_notice_bundle(NOTICE_SOURCE, stage, args.target)
        archive(stage, args.output, args)


def main(argv=None):
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("archive", "build"):
        command = sub.add_parser(name)
        command.add_argument("--source", required=True)
        command.add_argument("--output", required=True, type=Path)
        command.add_argument("--target", required=True, choices=sorted(TARGETS))
        command.add_argument("--version", required=True)
        command.add_argument("--revision")
        command.add_argument("--dirty", action="store_true")
        command.add_argument("--asset-pin", action="append", default=[])
        command.add_argument("--go-version", default="unknown")
        command.add_argument("--built-at", default="unknown")
    args = parser.parse_args(argv)
    try:
        if args.command == "archive":
            archive(args.source, args.output, args)
        else:
            build(args)
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        print(f"release: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
