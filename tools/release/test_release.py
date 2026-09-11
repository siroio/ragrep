import hashlib
import json
import subprocess
import sys
import tarfile
import zipfile
from pathlib import Path
import tempfile
import importlib.util
from types import SimpleNamespace


SCRIPT = Path(__file__).with_name("release.py")
MODULE_SPEC = importlib.util.spec_from_file_location("release", SCRIPT)
release = importlib.util.module_from_spec(MODULE_SPEC)
MODULE_SPEC.loader.exec_module(release)


def run(*args):
    return subprocess.run([sys.executable, str(SCRIPT), *args], text=True, capture_output=True)


def seed_required_notices(source, target):
    for relative in release.required_notice_paths(target):
        path = source / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(relative + "\n", encoding="utf-8")


def test_reproducible_archive_and_checksums(tmp_path):
    source = tmp_path / "source"
    (source / "nested").mkdir(parents=True)
    (source / "ragrep.exe").write_bytes(b"binary")
    (source / "LICENSE").write_text("license\n", encoding="utf-8")
    (source / "nested" / "INSTALL.md").write_text("install\n", encoding="utf-8")
    first = tmp_path / "one.zip"
    second = tmp_path / "two.zip"
    for output in (first, second):
        result = run("archive", "--source", str(source), "--output", str(output), "--target", "windows-amd64", "--version", "1.2.3")
        assert result.returncode == 0, result.stderr
        assert (output.with_suffix(".zip.sha256")).read_text(encoding="utf-8").split()[0] == hashlib.sha256(output.read_bytes()).hexdigest()
    assert first.read_bytes() == second.read_bytes()
    with zipfile.ZipFile(first) as archive:
        assert archive.namelist() == ["LICENSE", "nested/INSTALL.md", "ragrep.exe", "RELEASE.json"]
        assert (archive.getinfo("ragrep.exe").external_attr >> 16) & 0o777 == 0o755


def test_archive_refuses_existing_output(tmp_path):
    source = tmp_path / "source"
    source.mkdir()
    (source / "ragrep").write_bytes(b"binary")
    output = tmp_path / "release.zip"
    output.write_bytes(b"keep")
    result = run("archive", "--source", str(source), "--output", str(output), "--target", "linux-amd64", "--version", "1")
    assert result.returncode != 0
    assert output.read_bytes() == b"keep"


def test_archive_refuses_existing_checksum_sidecar(tmp_path):
    source = tmp_path / "source"
    source.mkdir()
    (source / "ragrep").write_bytes(b"binary")
    output = tmp_path / "release.zip"
    sidecar = output.with_name(output.name + ".sha256")
    sidecar.write_text("keep\n", encoding="ascii")
    result = run("archive", "--source", str(source), "--output", str(output), "--target", "linux-amd64", "--version", "1")
    assert result.returncode != 0
    assert not output.exists()
    assert sidecar.read_text(encoding="ascii") == "keep\n"


def test_metadata_records_dirty_revision_and_target(tmp_path):
    source = tmp_path / "source"
    source.mkdir()
    (source / "ragrep").write_bytes(b"binary")
    output = tmp_path / "release.zip"
    result = run("archive", "--source", str(source), "--output", str(output), "--target", "darwin-arm64", "--version", "1", "--revision", "abc", "--dirty")
    assert result.returncode == 0, result.stderr
    with zipfile.ZipFile(output) as archive:
        metadata = json.loads(archive.read("RELEASE.json"))
    assert metadata["revision"] == "abc"
    assert metadata["dirty"] is True
    assert metadata["target"] == "darwin-arm64"


def test_notice_bundle_rejects_missing_source(tmp_path):
    stage = tmp_path / "stage"
    stage.mkdir()
    stage_notice_bundle = getattr(release, "stage_notice_bundle", None)
    assert callable(stage_notice_bundle), "stage_notice_bundle is not implemented"
    try:
        stage_notice_bundle(tmp_path / "missing-notices", stage, "linux-amd64")
    except ValueError as exc:
        assert "notice source directory does not exist" in str(exc)
    else:
        raise AssertionError("missing notice source was accepted")


def test_notice_bundle_copies_common_and_target_only(tmp_path):
    source = tmp_path / "notices"
    seed_required_notices(source, "linux-amd64")
    (source / "native" / "windows-amd64").mkdir(parents=True)
    (source / "common.txt").write_bytes(b"common\x00")
    (source / "native" / "linux-amd64" / "LICENSE").write_bytes(b"linux")
    (source / "native" / "windows-amd64" / "LICENSE").write_bytes(b"windows")
    stage = tmp_path / "stage"
    stage.mkdir()
    release.stage_notice_bundle(source, stage, "linux-amd64")
    bundle = stage / "THIRD_PARTY_NOTICES"
    assert (bundle / "common.txt").read_bytes() == b"common\x00"
    assert (bundle / "native" / "linux-amd64" / "LICENSE").read_bytes() == b"linux"
    assert not (bundle / "native" / "windows-amd64").exists()


def test_notice_bundle_rejects_missing_required_file(tmp_path):
    source = tmp_path / "notices"
    source.mkdir()
    (source / "INVENTORY.txt").write_text("inventory\n", encoding="utf-8")
    stage = tmp_path / "stage"
    stage.mkdir()
    try:
        release.stage_notice_bundle(source, stage, "linux-amd64")
    except ValueError as exc:
        assert "required notice file is missing" in str(exc)
    else:
        raise AssertionError("missing required notice file was accepted")


def test_archive_includes_staged_notice_bundle(tmp_path):
    source = tmp_path / "notices"
    source.mkdir()
    seed_required_notices(source, "linux-amd64")
    (source / "gemma" / "Notice").write_text(
        "Gemma is provided under and subject to the Gemma Terms of Use found at ai.google.dev/gemma/terms\n",
        encoding="utf-8",
    )
    stage = tmp_path / "stage"
    stage.mkdir()
    (stage / "ragrep").write_bytes(b"binary")
    release.stage_notice_bundle(source, stage, "linux-amd64")
    output = tmp_path / "release.zip"
    release.archive(
        stage,
        output,
        SimpleNamespace(version="1", revision="r", dirty=False, go_version="go", target="linux-amd64", asset_pin=[], built_at="t"),
    )
    with zipfile.ZipFile(output) as archive:
        assert archive.read("THIRD_PARTY_NOTICES/gemma/Notice").startswith(b"Gemma is provided")


if __name__ == "__main__":
    # Keep this runnable without a test framework, as required by the release check.
    failures = []
    tests = (test_reproducible_archive_and_checksums, test_archive_refuses_existing_output, test_archive_refuses_existing_checksum_sidecar, test_metadata_records_dirty_revision_and_target, test_notice_bundle_rejects_missing_source, test_notice_bundle_copies_common_and_target_only, test_notice_bundle_rejects_missing_required_file, test_archive_includes_staged_notice_bundle)
    for test in tests:
        with tempfile.TemporaryDirectory() as directory:
            try:
                test(Path(directory))
            except Exception as exc:
                failures.append(f"{test.__name__}: {exc}")
    if failures:
        raise SystemExit("\n".join(failures))
    print(f"release tests: {len(tests)} passed")
