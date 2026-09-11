#!/usr/bin/env python3
"""Run a bounded CLI and stdio-MCP smoke against an extracted release binary."""

import argparse
import json
import os
import shutil
import subprocess
import tempfile
import threading
import queue
import time
from pathlib import Path


MAX_MCP_OUTPUT = 8 * 1024 * 1024
MAX_MCP_LINE = 1024 * 1024


def system_path(environment=None):
    environment = environment or os.environ
    if os.name == "nt":
        return str(Path(environment.get("SystemRoot", r"C:\Windows")) / "System32")
    return ""


def copy_asset_cache(source, destination):
    source = Path(source).resolve()
    destination = Path(destination).resolve()
    asset_root = source / "ragrep" if (source / "ragrep").is_dir() else source
    if not asset_root.is_dir():
        raise ValueError(f"asset cache does not exist: {source}")
    root_assets = {"model.onnx", "model.onnx_data", "model_quantized.onnx", "model_quantized.onnx_data", "tokenizer.model", "onnxruntime.dll", "onnxruntime.so", "libonnxruntime.so", "libonnxruntime.dylib", "DirectML.dll"}
    for path in asset_root.rglob("*"):
        relative = path.relative_to(asset_root)
        allowed = path.name in root_assets or (len(relative.parts) == 2 and relative.parts[0].startswith("ort-") and path.name in root_assets)
        if not path.is_file() or not allowed:
            continue
        target = destination / "ragrep" / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(path, target)


def isolated_environment(root):
    root = Path(root).resolve()
    cache = root / "cache"
    config = root / "config"
    local_appdata = cache
    appdata = config
    home = root / "home"
    env = os.environ.copy()
    env.update({
        "XDG_CACHE_HOME": str(cache),
        "XDG_CONFIG_HOME": str(config),
        "LOCALAPPDATA": str(local_appdata),
        "APPDATA": str(appdata),
        "USERPROFILE": str(home),
        "HOME": str(home),
        "PATH": system_path(env),
        "RAGREP_DAEMON_ADDR": "127.0.0.1:0",
    })
    return env, cache


def mcp_exchange(process, timeout=30):
    messages = [
        {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "smoke", "version": "1"}}},
        {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}},
        {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}},
        {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "search_documents", "arguments": {"query": "needle", "mode": "text"}}},
    ]
    payload = b"".join((json.dumps(message) + "\n").encode() for message in messages)
    if not hasattr(process, "stdin"):
        return _communicate_exchange(process, payload, timeout)
    lines = queue.Queue()

    def collect():
        try:
            for line in process.stdout:
                lines.put(line)
        finally:
            lines.put(None)

    reader = threading.Thread(target=collect, daemon=True)
    reader.start()
    try:
        process.stdin.write(payload)
        process.stdin.flush()
        responses = {}
        total = 0
        deadline = time.monotonic() + timeout
        while any(request_id not in responses for request_id in (1, 2, 3)):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise RuntimeError(f"MCP exchange timed out after {timeout}s")
            try:
                line = lines.get(timeout=remaining)
            except queue.Empty as exc:
                raise RuntimeError(f"MCP exchange timed out after {timeout}s") from exc
            if line is None:
                raise RuntimeError("MCP process closed before all responses")
            total += len(line)
            _record_mcp_response(line, responses, total)
    except (BrokenPipeError, RuntimeError):
        _terminate_process(process)
        raise
    finally:
        if process.stdin and not process.stdin.closed:
            process.stdin.close()
    _terminate_process(process)
    return responses


def _communicate_exchange(process, payload, timeout):
    try:
        stdout, _ = process.communicate(payload, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        process.kill()
        try:
            process.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.communicate()
        raise RuntimeError(f"MCP exchange timed out after {timeout}s") from exc
    if len(stdout) > MAX_MCP_OUTPUT:
        raise RuntimeError("MCP output exceeds limit")
    responses = {}
    total = 0
    for line in stdout.splitlines(keepends=True):
        total += len(line)
        _record_mcp_response(line, responses, total)
    missing = [request_id for request_id in (1, 2, 3) if request_id not in responses]
    if missing:
        raise RuntimeError(f"MCP response missing id(s): {missing}")
    return responses


def _record_mcp_response(line, responses, total):
    if total > MAX_MCP_OUTPUT or len(line) > MAX_MCP_LINE:
        raise RuntimeError("MCP response exceeds limit")
    try:
        response = json.loads(line)
    except json.JSONDecodeError as exc:
        raise RuntimeError("MCP malformed JSON response") from exc
    if not isinstance(response, dict):
        raise RuntimeError("MCP malformed JSON response")
    request_id = response.get("id")
    if request_id in (1, 2, 3):
        result = response.get("result")
        if "error" in response or (isinstance(result, dict) and result.get("isError")):
            raise RuntimeError("MCP protocol error")
        responses[request_id] = response


def _terminate_process(process):
    if not hasattr(process, "poll") or process.poll() is None:
        process.kill()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def stop_own_daemon(binary, root, env):
    try:
        subprocess.run(
            [str(binary), "daemon", "stop"],
            cwd=root,
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
            timeout=10,
        )
    except (OSError, subprocess.TimeoutExpired):
        pass


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--mcp-response", required=True)
    parser.add_argument("--mcp-tools-response", required=True)
    parser.add_argument("--asset-cache")
    args = parser.parse_args(argv)
    binary = Path(args.binary).resolve()
    if not binary.is_file():
        raise SystemExit(f"missing binary: {binary}")
    with tempfile.TemporaryDirectory(prefix="ragrep-smoke-") as temp:
        base = Path(temp)
        root = base / "workspace"
        root.mkdir()
        (root / "notes.md").write_text("needle authentication\n", encoding="utf-8")
        env, cache = isolated_environment(base)
        if args.asset_cache:
            copy_asset_cache(args.asset_cache, cache)
        env.setdefault("RAG_DOWNLOAD", "1")
        process = None
        try:
            db = root / ".ragrep" / "index.db"
            subprocess.run([str(binary), "init", "--db", str(db)], cwd=root, env=env, check=True, timeout=120)
            subprocess.run([str(binary), "index", "--db", str(db), str(root)], cwd=root, env=env, check=True, timeout=120)
            result = subprocess.run([str(binary), "search", "--json", "--mode", "text", "--db", str(db), "needle"], cwd=root, env=env, check=True, capture_output=True, text=True, timeout=30)
            hits = json.loads(result.stdout)
            if not any(hit.get("doc") == "notes.md" and "needle" in hit.get("snippet", "") for hit in hits):
                raise SystemExit("CLI smoke: missing expected hit")
            process = subprocess.Popen([str(binary), "mcp", "serve"], cwd=root, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            responses = mcp_exchange(process)
            tools = responses[2]["result"].get("tools", [])
            if len(tools) != 9:
                raise SystemExit(f"MCP smoke: expected 9 tools, got {len(tools)}")
            response = responses[3]
            Path(args.mcp_response).write_text(json.dumps(response), encoding="utf-8")
            Path(args.mcp_tools_response).write_text(json.dumps(responses[2]), encoding="utf-8")
            data = response["result"].get("structuredContent", {}).get("data", {})
            if not any(hit.get("path") == "notes.md" and "needle" in hit.get("snippet", "") for hit in data.get("hits", [])):
                raise SystemExit("MCP smoke: missing expected hit")
        finally:
            if process is not None and process.poll() is None:
                process.kill()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
            stop_own_daemon(binary, root, env)
    print("staged CLI/MCP smoke: passed")


if __name__ == "__main__":
    main()
