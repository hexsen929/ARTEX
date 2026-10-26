#!/usr/bin/env python3
"""Cloud acceptance checks: CLI help and empty-session loopback MCP only."""
import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(url, token, method, params=None, identifier=1):
    data = json.dumps({"jsonrpc": "2.0", "id": identifier, "method": method, "params": params or {}}).encode()
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json", "Authorization": "Bearer " + token})
    with urllib.request.urlopen(req, timeout=3) as response:
        result = json.load(response)
    if "error" in result:
        raise RuntimeError("MCP returned a JSON-RPC error: " + str(result["error"]))
    return result["result"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tools-dir", type=Path, required=True)
    args = parser.parse_args()
    tools = args.tools_dir.absolute()
    env = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1"}
    for name in ("impacket", "nxc", "pypykatz", "lazagne-macos", "penelope"):
        subprocess.run([str(tools / name), "--help"], env=env, check=True, timeout=60, stdout=subprocess.DEVNULL)
        print("CLI_HELP_OK", name, flush=True)
    with tempfile.TemporaryDirectory(prefix="artex-penelope-smoke-") as temporary:
        root = Path(temporary)
        token = secrets.token_hex(24)
        listen_port, mcp_port = free_port(), free_port()
        while listen_port == mcp_port:
            mcp_port = free_port()
        env["ARTEX_PENELOPE_DATA_DIR"] = str(root / "state")
        with (root / "handler.log").open("wb") as log:
            process = subprocess.Popen([str(tools / "penelope"), "--mcp", "--mcp-host", "127.0.0.1", "--mcp-port", str(mcp_port), "--mcp-token", token, "-p", str(listen_port), "-i", "127.0.0.1", "-C"], stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT, env=env)
            try:
                url = "http://127.0.0.1:" + str(mcp_port) + "/"
                deadline = time.monotonic() + 20
                while True:
                    if process.poll() is not None:
                        raise RuntimeError("Penelope exited before MCP readiness")
                    try:
                        result = request(url, token, "initialize", {"protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "artex-package-smoke", "version": "1"}})
                        if not result.get("capabilities", {}).get("tools") == {}:
                            raise RuntimeError("Penelope did not advertise MCP tools")
                        break
                    except (OSError, urllib.error.URLError):
                        if time.monotonic() >= deadline:
                            raise RuntimeError("Penelope MCP readiness timeout")
                        time.sleep(0.2)
                try:
                    request(url, "wrong-token", "tools/list")
                    raise RuntimeError("Penelope accepted an invalid MCP token")
                except urllib.error.HTTPError as error:
                    if error.code != 401:
                        raise
                listed = request(url, token, "tools/list")
                names = {entry["name"] for entry in listed["tools"]}
                expected = {"list_sessions", "get_session_info", "exec_in_session", "kill_session", "upload_to_session", "download_from_session"}
                if not expected.issubset(names):
                    raise RuntimeError("Penelope MCP tool schema is incompatible")
                sessions = request(url, token, "tools/call", {"name": "list_sessions", "arguments": {}})
                payload = json.loads(sessions["content"][0]["text"])
                if payload != {"sessions": []}:
                    raise RuntimeError("Expected an empty, isolated session handler")
                # Querying the listener does not create a shell connection.
                subprocess.run(["lsof", "-nP", "-a", "-p", str(process.pid), "-iTCP:" + str(listen_port), "-sTCP:LISTEN"], check=True, stdout=subprocess.DEVNULL, timeout=5)
                print("PENELOPE_MCP_OK authenticated initialize/tools-list/empty-sessions; listener bound; no target operations", flush=True)
            except Exception:
                log.flush()
                diagnostic = (root / "handler.log").read_text(errors="replace")[-6000:].replace(token, "<ephemeral-token>")
                print(diagnostic)
                raise
            finally:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


if __name__ == "__main__":
    main()
