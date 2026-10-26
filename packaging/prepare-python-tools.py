#!/usr/bin/env python3
"""Prepare ARTEX Python tools without compiling dependencies on the user's Mac.

The default prints a plan. --build-wheelhouse is allowed only in GitHub Actions;
it builds fixed sources and records every wheel hash. --install consumes that
wheelhouse offline, uses separate Python 3.11 venvs, and never replaces custom
files. No mode runs credential collection, network probes, or target commands.
"""
from __future__ import annotations

import argparse
import ast
import email.parser
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import urllib.request
import zipfile


ROOT = Path(__file__).resolve().parent
SOURCE_PATH = ROOT / "python-tools-sources.json"
GROUPS = ("impacket", "nxc", "pypykatz", "lazagne-macos", "penelope")
PENELOPE_ADAPTER_REVISION = 3
PURPOSES = {
    "impacket": "Windows protocol tool collection; invoke impacket <script.py> [arguments].",
    "nxc": "NetExec protocol test CLI in an isolated Python environment.",
    "pypykatz": "Offline Windows memory/credential artifact parser; does not capture host memory.",
    "lazagne-macos": "macOS target-side LaZagne source resource; modern OS support requires separate validation.",
    "penelope": "Managed session handler with ARTEX-compatible authenticated loopback HTTP MCP.",
}


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def canonical(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


def host_platform() -> str:
    return {"Darwin": "darwin", "Linux": "linux", "Windows": "windows"}.get(platform.system(), platform.system().lower()) + "/" + {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine(), platform.machine())


def fail(message: str) -> None:
    raise RuntimeError(message)


def safe_path(path: Path) -> None:
    for parent in (path, *path.parents):
        if parent.is_symlink():
            fail("Symlink destination ancestor: " + str(parent))


def atomic_bytes(path: Path, data: bytes, mode: int = 0o644, *, owned: bool = False) -> None:
    safe_path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        if path.is_file() and path.read_bytes() == data:
            return
        if not owned:
            fail("Preserving an existing custom file: " + str(path))
    fd, temporary = tempfile.mkstemp(prefix=".artex-python-", dir=path.parent)
    temporary_path = Path(temporary)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fchmod(stream.fileno(), mode)
            os.fsync(stream.fileno())
        os.replace(temporary_path, path)
    finally:
        temporary_path.unlink(missing_ok=True)


def write_json(path: Path, obj: object, *, owned: bool = False) -> None:
    atomic_bytes(path, (json.dumps(obj, ensure_ascii=False, indent=2) + "\n").encode(), owned=owned)


def fetch(url: str, expected: str) -> bytes:
    request = urllib.request.Request(url, headers={"User-Agent": "ARTEX-tools-preparer/1"})
    with urllib.request.urlopen(request, timeout=90) as response:
        data = response.read(64 * 1024 * 1024 + 1)
    if len(data) > 64 * 1024 * 1024 or hashlib.sha256(data).hexdigest() != expected:
        fail("Source checksum or size rejected: " + url)
    return data


def unpack_source_zip(data: bytes, destination: Path) -> Path:
    archive = zipfile.ZipFile(io.BytesIO(data))
    total = 0
    roots = set()
    for entry in archive.infolist():
        path = PurePosixPath(entry.filename)
        total += entry.file_size
        if path.is_absolute() or ".." in path.parts or not path.parts or total > 256 * 1024 * 1024:
            fail("Unsafe source archive member")
        if stat.S_ISLNK(entry.external_attr >> 16):
            fail("Source archive contains a symlink")
        roots.add(path.parts[0])
    if len(roots) != 1:
        fail("Expected one source archive root")
    archive.extractall(destination)
    return destination / next(iter(roots))


def source_zip(spec: dict, destination: Path) -> Path:
    url = "https://codeload.github.com/" + spec["repository"] + "/zip/" + spec["commit"]
    return unpack_source_zip(fetch(url, spec["sha256"]), destination)


def resource_spec(name: str, sources: dict) -> tuple[str, str, str]:
    spec = sources["sources"][name]
    if name == "penelope":
        return ("resources/penelope.py", "https://raw.githubusercontent.com/" + spec["repository"] + "/" + spec["commit"] + "/penelope.py", spec["sha256"])
    if name == "lazagne-macos":
        return ("resources/lazagne-macos.zip", "https://codeload.github.com/" + spec["repository"] + "/zip/" + spec["commit"], spec["sha256"])
    fail("Unknown source resource: " + name)


def resource_bytes(name: str, wheelhouse: Path | None, index: dict, sources: dict) -> bytes:
    relative, url, expected = resource_spec(name, sources)
    if wheelhouse is None:
        return fetch(url, expected)
    record = index.get("resources", {}).get(name)
    if not record or record.get("file") != relative or record.get("sha256") != expected:
        fail("Offline source resource is missing or has different provenance: " + name)
    path = wheelhouse / relative
    safe_path(path)
    if not path.is_file() or digest(path) != expected:
        fail("Offline source resource checksum mismatch: " + name)
    return path.read_bytes()


def run(command: list[str], *, cwd: Path | None = None, env: dict | None = None) -> None:
    print("RUN", shlex.join(command), flush=True)
    subprocess.run(command, cwd=cwd, env=env, check=True)


def python_version(python: str) -> tuple[int, int]:
    output = subprocess.check_output([python, "-I", "-c", "import sys;print(str(sys.version_info[0])+'.'+str(sys.version_info[1]))"], text=True)
    return tuple(map(int, output.strip().split(".")))


def wheel_info(path: Path) -> tuple[str, str]:
    with zipfile.ZipFile(path) as archive:
        names = [name for name in archive.namelist() if name.endswith(".dist-info/METADATA")]
        if len(names) != 1:
            fail("Invalid wheel metadata: " + path.name)
        metadata = email.parser.BytesParser().parsebytes(archive.read(names[0]))
        for dependency in metadata.get_all("Requires-Dist", []):
            if "@" in dependency:
                fail("Unresolved direct reference in wheel " + path.name + ": " + dependency)
        return metadata["Name"], metadata["Version"]


def build_wheelhouse(args: argparse.Namespace, sources: dict) -> None:
    if os.environ.get("GITHUB_ACTIONS") != "true":
        fail("Source builds run in GitHub Actions; local installation accepts wheels only.")
    if host_platform() != sources["platform"] or python_version(args.python) != (3, 11):
        fail("Wheelhouse requires a native macOS ARM64 Python 3.11 runner.")
    destination = args.wheelhouse.absolute()
    safe_path(destination)
    if destination.exists() and any(destination.iterdir()):
        fail("Wheelhouse output must be new or empty")
    destination.mkdir(parents=True, exist_ok=True)
    index = {"schema": 1, "platform": host_platform(), "python": "3.11", "sources_sha256": digest(SOURCE_PATH), "groups": {}, "sources": sources["sources"], "resources": {}}
    for name in ("penelope", "lazagne-macos"):
        if name in args.tools:
            relative, url, expected = resource_spec(name, sources)
            atomic_bytes(destination / relative, fetch(url, expected))
            index["resources"][name] = {"file": relative, "sha256": expected, "source_url": url}
    pip = [args.python, "-m", "pip", "--disable-pip-version-check", "--no-cache-dir"]
    with tempfile.TemporaryDirectory(prefix="artex-python-sources-") as temporary:
        temporary_path = Path(temporary)
        pinned = temporary_path / "pinned-wheels"
        pinned.mkdir()
        if "nxc" in args.tools:
            for name in ("certipy-nxc", "impacket-nxc", "oscrypto", "pynfsclient"):
                spec = sources["sources"][name]
                unpacked = source_zip(spec, temporary_path / name)
                run(pip + ["wheel", "--no-deps", "--wheel-dir", str(pinned), str(unpacked)], cwd=unpacked)
                licenses = destination / "licenses" / name
                licenses.mkdir(parents=True, exist_ok=True)
                for p in unpacked.iterdir():
                    if p.is_file() and p.name.upper().startswith(("LICENSE", "COPYING")):
                        shutil.copyfile(p, licenses / p.name)
            nxc_source = source_zip(sources["sources"]["netexec"], temporary_path / "nxc")
            pyproject = nxc_source / "pyproject.toml"
            original = pyproject.read_text()
            text = original
            pinned_specs = {}
            for wheel in pinned.glob("*.whl"):
                name, version = wheel_info(wheel)
                pinned_specs[canonical(name)] = name + "==" + version
            dependencies = {
                'certipy-ad @ git+https://github.com/Pennyw0rth/Certipy': pinned_specs["certipy-nxc"],
                'impacket @ git+https://github.com/fortra/impacket': pinned_specs["impacket"],
                'oscrypto @ git+https://github.com/wbond/oscrypto': pinned_specs["oscrypto"],
                'pynfsclient @ git+https://github.com/Pennyw0rth/NfsClient': pinned_specs["pynfsclient"],
            }
            for before, after in dependencies.items():
                if text.count(before) != 1:
                    fail("NetExec packaging dependency changed: " + before)
                text = text.replace(before, after)
            text = text.replace('version = "0.0.0" # Poetry placeholder, do not remove', 'version = "1.5.1" # ARTEX fixed source version')
            text = text.replace('enable = true\nstyle = "pep440"', 'enable = false\nstyle = "pep440"')
            pyproject.write_text(text)
            index["netexec_packaging_patch"] = {"before_sha256": hashlib.sha256(original.encode()).hexdigest(), "after_sha256": hashlib.sha256(text.encode()).hexdigest(), "dependencies": dependencies, "note": "Fixed upstream Git dependency commits; Certipy fork distribution is certipy-nxc. No protocol dependencies removed."}
        for name in args.tools:
            if name == "penelope":
                continue
            group = destination / name
            group.mkdir()
            specifications = sources["packages"].get(name, [])
            command = pip + ["wheel", "--wheel-dir", str(group)]
            if name == "nxc":
                command += [str(nxc_source)] + [str(p) for p in sorted(pinned.glob("*.whl"))]
            else:
                command += specifications
            run(command)
            records = []
            names = set()
            for wheel in sorted(group.glob("*.whl")):
                package, version = wheel_info(wheel)
                if canonical(package) in names:
                    fail("Wheelhouse contains multiple versions of " + package)
                names.add(canonical(package))
                records.append({"file": wheel.name, "name": package, "version": version, "sha256": digest(wheel)})
            if not records:
                fail("Empty wheel group: " + name)
            requirements = "\n".join(r["name"] + "==" + r["version"] + " --hash=sha256:" + r["sha256"] for r in records) + "\n"
            (group / "requirements.lock").write_text(requirements)
            index["groups"][name] = {"wheels": records, "requirements_sha256": digest(group / "requirements.lock")}
        write_json(destination / "wheelhouse-index.json", index)
    print(json.dumps({"wheelhouse": str(destination), "groups": list(index["groups"]), "source_builds": "GitHub Actions only"}), flush=True)


def validate_wheelhouse(path: Path, sources: dict, names: list[str]) -> dict:
    safe_path(path)
    index = json.loads((path / "wheelhouse-index.json").read_text())
    if index["platform"] != host_platform() or index["python"] != "3.11" or index["sources_sha256"] != digest(SOURCE_PATH):
        fail("Wheelhouse provenance, platform, or Python version mismatch")
    for name in names:
        if name in ("penelope", "lazagne-macos"):
            resource_bytes(name, path, index, sources)
        if name == "penelope":
            continue
        group = index["groups"].get(name)
        if not group:
            fail("Wheelhouse group missing: " + name)
        directory = path / name
        expected = {r["file"] for r in group["wheels"]} | {"requirements.lock"}
        if {p.name for p in directory.iterdir()} != expected:
            fail("Unexpected wheelhouse contents: " + name)
        if digest(directory / "requirements.lock") != group["requirements_sha256"]:
            fail("Wheel requirement lock checksum mismatch")
        for record in group["wheels"]:
            if PurePosixPath(record["file"]).name != record["file"] or not record["file"].endswith(".whl"):
                fail("Invalid wheel filename")
            p = directory / record["file"]
            safe_path(p)
            if digest(p) != record["sha256"]:
                fail("Wheel checksum mismatch: " + p.name)
    return index


def make_wrapper(python: Path, entry: Path, defaults: dict[str, str] | None = None) -> bytes:
    text = "#!/bin/sh\nexport PYTHONDONTWRITEBYTECODE=1\n"
    for name, value in (defaults or {}).items():
        if not re.fullmatch(r"ARTEX_[A-Z_]+|NXC_PATH", name):
            fail("Unsupported runtime-directory variable")
        text += 'if [ -z "${' + name + ':-}" ]; then\n  ' + name + "=" + shlex.quote(value) + "\n  export " + name + "\nfi\n"
    return (text + "exec " + shlex.quote(str(python)) + " -B " + shlex.quote(str(entry)) + ' "$@"\n').encode()


def impacket_wrapper(python: Path, bindir: Path) -> tuple[bytes, list[str]]:
    commands = sorted(p.name for p in bindir.glob("*.py") if p.is_file())
    if "smbclient.py" not in commands:
        fail("Impacket's expected console scripts are missing")
    text = '#!/bin/sh\nexport PYTHONDONTWRITEBYTECODE=1\nif [ "$#" -eq 0 ] || [ "$1" = "--help" ]; then\n'
    text += "  printf '%s\\n' " + shlex.quote("Usage: impacket <script.py> [arguments]\nAvailable: " + ", ".join(commands)) + "\n  exit 0\nfi\n"
    text += 'case "$1" in\n  ' + "|".join(commands) + ') tool="$1"; shift ;;\n  *) printf \'%s\\n\' "Unknown Impacket script: $1" >&2; exit 2 ;;\nesac\n'
    text += "exec " + shlex.quote(str(python)) + " -B " + shlex.quote(str(bindir)) + '/"$tool" "$@"\n'
    return text.encode(), commands


def manifest_entry(record: dict) -> dict:
    fields = ("name", "purpose", "version", "platform", "sha256", "path", "execution", "resolved_executable", "resolved_sha256")
    return {key: record[key] for key in fields if key in record}


def adapt_penelope(upstream: bytes) -> bytes:
    original_line = b"self.basedir = globals().get('_ephemeral_root') or (real_home / f'.{__program__}')"
    replacement_line = b"self.basedir = Path(os.environ['ARTEX_PENELOPE_DATA_DIR']) if os.environ.get('ARTEX_PENELOPE_DATA_DIR') else (globals().get('_ephemeral_root') or (real_home / f'.{__program__}'))"
    main_guard = b'if __name__ == "__main__":\n\tmain()'
    listener_tail = b'\t\tlistener_menu()\n\t\tsignal.signal(signal.SIGINT, keyboard_interrupt)\n\t\tmenu.start()'
    if upstream.count(original_line) != 1 or upstream.count(main_guard) != 1 or upstream.count(listener_tail) != 1:
        fail("Penelope source no longer matches the reviewed adapter")
    diagnostic = b'''if os.environ.get('ARTEX_PENELOPE_STARTUP_DIAGNOSTICS') == '1':
\timport faulthandler
\tfaulthandler.enable()
\tfaulthandler.dump_traceback_later(8, repeat=True)
\tif hasattr(signal, 'SIGUSR2'):
\t\tfaulthandler.register(signal.SIGUSR2, all_threads=True)

'''
    headless_tail = b'''\t\t# ARTEX manages this MCP handler without a controlling terminal.
\t\t# -C disables session attachment, but upstream listener_menu still calls
\t\t# tty.setraw unconditionally. Keep the Core and MCP threads alive while
\t\t# retaining main()'s existing SIGINT/SIGTERM shutdown handlers.
\t\tif options.mcp and not sys.stdin.isatty():
\t\t\twhile core.started:
\t\t\t\ttime.sleep(0.2)
\t\telse:
\t\t\tlistener_menu()
\t\t\tsignal.signal(signal.SIGINT, keyboard_interrupt)
\t\t\tmenu.start()'''
    patched = upstream.replace(original_line, replacement_line).replace(listener_tail, headless_tail).replace(main_guard, diagnostic + main_guard)
    ast.parse(patched.decode())
    return patched


def runtime_defaults(name: str, destination: Path) -> dict[str, str]:
    if name == "nxc":
        return {"NXC_PATH": str(destination / "runtime" / "nxc")}
    if name == "penelope":
        return {"ARTEX_PENELOPE_DATA_DIR": str(destination.parent / "reverse" / "penelope")}
    return {}


def ensure_runtime_parents(name: str, destination: Path) -> None:
    # NetExec creates its own leaf with os.mkdir, so its parent must exist first.
    # Leave the leaf to the tool's first-run setup and preserve environment
    # overrides; no HOME changes or credential/session operations occur here.
    for value in runtime_defaults(name, destination).values():
        parent = Path(value).parent
        safe_path(parent)
        parent.mkdir(mode=0o700, parents=True, exist_ok=True)


def expected_version(name: str, sources: dict) -> str:
    if name == "penelope":
        return sources["sources"]["penelope"]["commit"]
    if name == "nxc":
        return sources["sources"]["netexec"]["version"]
    if name == "lazagne-macos":
        return sources["sources"][name]["version"]
    return sources["packages"][name][0].split("==", 1)[1]


def verify_existing_record(record: dict, destination: Path, sources: dict, index: dict) -> None:
    name = record["name"]
    if record.get("sources_sha256") != digest(SOURCE_PATH) or record.get("version") != expected_version(name, sources):
        fail("Existing tool uses a different source/version; preserve it and prepare a new destination: " + name)
    if name != "penelope" and record.get("wheelhouse_group") != index["groups"].get(name):
        fail("Existing tool uses a different wheel lock; preserve it and prepare a new destination: " + name)
    if name == "penelope" and record.get("adapter_revision") != PENELOPE_ADAPTER_REVISION:
        fail("Existing Penelope uses a different adapter; preserve it and prepare a new destination")
    for path_key, hash_key in (("path", "sha256"), ("resolved_executable", "resolved_sha256")):
        if path_key not in record:
            fail("Existing Python record lacks entry provenance: " + name)
        relative = PurePosixPath(record[path_key])
        if relative.is_absolute() or ".." in relative.parts:
            fail("Unsafe existing Python record path")
        path = destination.parent / relative
        safe_path(path)
        if not path.resolve().is_relative_to(destination.resolve()) or not path.is_file() or digest(path) != record[hash_key]:
            fail("Existing installed Python tool changed: " + name)


def install(args: argparse.Namespace, sources: dict) -> None:
    if host_platform() != "darwin/arm64" or python_version(args.python) != (3, 11):
        fail("This deployment profile requires native Mac ARM64 Python 3.11")
    destination = args.destination.absolute()
    safe_path(destination)
    destination.mkdir(parents=True, exist_ok=True)
    index = validate_wheelhouse(args.wheelhouse.absolute(), sources, args.tools) if args.wheelhouse is not None else {}
    lock_path = destination / "python-tools-install.lock.json"
    previous = json.loads(lock_path.read_text()) if lock_path.exists() else {"schema": 1, "host_platform": host_platform(), "records": []}
    if previous.get("schema") != 1 or previous.get("host_platform") != host_platform():
        fail("Existing Python installation lock does not match this host")
    records = {record["name"]: record for record in previous["records"]}
    entries = []
    for name in args.tools:
        ensure_runtime_parents(name, destination)
        if name in records:
            record = records[name]
            verify_existing_record(record, destination, sources, index)
            entries.append(manifest_entry(record))
            print("UNCHANGED", name, flush=True)
            continue
        environment = destination / "python-envs" / name
        safe_path(environment)
        if environment.exists():
            fail("Preserving existing unowned Python environment: " + str(environment))
        created = False
        try:
            run([args.python, "-m", "venv", str(environment)])
            created = True
            python = environment / "bin" / "python"
            if name != "penelope":
                group = args.wheelhouse.absolute() / name
                run([str(python), "-m", "pip", "--disable-pip-version-check", "--no-cache-dir", "install", "--no-compile", "--only-binary=:all:", "--no-index", "--find-links", str(group), "--require-hashes", "-r", str(group / "requirements.lock")])
                run([str(python), "-m", "pip", "check"])
            commands = []
            execution = "host"
            if name == "penelope":
                spec = sources["sources"]["penelope"]
                url = "https://raw.githubusercontent.com/" + spec["repository"] + "/" + spec["commit"] + "/penelope.py"
                upstream = resource_bytes(name, args.wheelhouse.absolute() if args.wheelhouse is not None else None, index, sources)
                ast.parse(upstream.decode())
                if not all(token in upstream for token in (b'class MCPServer:', b'--mcp-token', b'list_sessions', b'get_session_info', b'exec_in_session')):
                    fail("Pinned Penelope is missing ARTEX's MCP interface")
                patched = adapt_penelope(upstream)
                source = destination / "sources" / ("penelope-" + spec["commit"]) / "penelope.py"
                atomic_bytes(source, upstream)
                entry = destination / "penelope.py"
                atomic_bytes(entry, patched, 0o755)
                wrapper = destination / "penelope"
                atomic_bytes(wrapper, make_wrapper(python, entry, runtime_defaults(name, destination)), 0o755)
                version = spec["commit"]
            elif name == "lazagne-macos":
                spec = sources["sources"][name]
                with tempfile.TemporaryDirectory(prefix="artex-lazagne-") as temporary:
                    source = unpack_source_zip(resource_bytes(name, args.wheelhouse.absolute(), index, sources), Path(temporary))
                    base = destination / "targets" / "darwin-arm64" / "lazagne"
                    for p in (source / "Mac").rglob("*"):
                        if p.is_file():
                            atomic_bytes(base / p.relative_to(source / "Mac"), p.read_bytes())
                    for p in source.iterdir():
                        if p.is_file() and p.name.upper().startswith(("LICENSE", "COPYING")):
                            atomic_bytes(base / p.name, p.read_bytes())
                entry = base / "laZagne.py"
                wrapper = destination / "lazagne-macos"
                atomic_bytes(wrapper, make_wrapper(python, entry), 0o755)
                execution = "target"
                version = spec["version"]
            else:
                bindir = environment / "bin"
                wrapper = destination / name
                if name == "impacket":
                    entry = bindir / "smbclient.py"
                    content, commands = impacket_wrapper(python, bindir)
                else:
                    entry = bindir / name
                    if not entry.is_file():
                        fail("Installed tool console entry missing: " + str(entry))
                    content = make_wrapper(python, entry, runtime_defaults(name, destination))
                atomic_bytes(wrapper, content, 0o755)
                version = expected_version(name, sources)
            record = {"name": name, "purpose": PURPOSES[name], "execution": execution, "platform": host_platform(), "path": wrapper.relative_to(destination.parent).as_posix(), "sha256": digest(wrapper), "version": version, "resolved_executable": entry.relative_to(destination.parent).as_posix(), "resolved_sha256": digest(entry), "python_executable": str(python), "source_url": "https://github.com/" + sources["sources"]["penelope" if name == "penelope" else "netexec" if name == "nxc" else "lazagne-macos"]["repository"] if name in ("penelope", "nxc", "lazagne-macos") else "https://pypi.org/project/" + name + "/" + version + "/", "sources_sha256": digest(SOURCE_PATH)}
            if name != "penelope":
                record["wheelhouse_group"] = index["groups"][name]
                record["wheelhouse_index_sha256"] = digest(args.wheelhouse.absolute() / "wheelhouse-index.json")
            else:
                record["upstream_sha256"] = spec["sha256"]
                record["installed_script_sha256"] = digest(entry)
                record["adapter_revision"] = PENELOPE_ADAPTER_REVISION
                record["adapter"] = "ARTEX data-directory override; MCP without a TTY retains Core/MCP lifecycle without the interactive menu; opt-in startup stack diagnostics."
            if commands:
                record["commands"] = commands
            if execution == "target":
                record["runtime_validation"] = "Source and dependencies prepared; no credential collection executed; compatibility with modern macOS requires target-specific validation."
            records[name] = record
            entries.append(manifest_entry(record))
            write_json(lock_path, {"schema": 1, "host_platform": host_platform(), "records": list(records.values())}, owned=True)
            print("INSTALLED", name, execution, flush=True)
        except Exception:
            if created:
                shutil.rmtree(environment)
            raise
    # Reconstruct all entries from the owned lock so subset reruns retain tools.
    entries = [manifest_entry(record) for record in records.values()]
    write_json(destination / "python-tools-manifest.local.json", {"tools": entries}, owned=True)
    print(json.dumps({"installed": [r["name"] for r in records.values()], "manifest": str(destination / "python-tools-manifest.local.json"), "source_builds_on_local_host": False}), flush=True)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--plan", action="store_true", help="Print the installation plan (default)")
    modes.add_argument("--build-wheelhouse", action="store_true", help="Build wheel artifacts in native GitHub Actions only")
    modes.add_argument("--install", action="store_true", help="Install the verified wheelhouse offline into isolated venvs")
    parser.add_argument("--destination", type=Path, help="ARTEX data/tools directory")
    parser.add_argument("--wheelhouse", type=Path, help="Cloud output or downloaded verified wheelhouse directory")
    parser.add_argument("--python", default=sys.executable, help="Python 3.11 executable")
    parser.add_argument("--tools", default=",".join(GROUPS), help="Comma-separated selection: " + ",".join(GROUPS))
    args = parser.parse_args()
    args.tools = list(dict.fromkeys(filter(None, args.tools.split(","))))
    if not args.tools or any(name not in GROUPS for name in args.tools):
        parser.error("Unknown or empty tool selection")
    if args.build_wheelhouse and args.wheelhouse is None:
        parser.error("--build-wheelhouse requires --wheelhouse")
    if args.install and (args.destination is None or (any(name != "penelope" for name in args.tools) and args.wheelhouse is None)):
        parser.error("--install requires --destination and a cloud --wheelhouse")
    sources = json.loads(SOURCE_PATH.read_text())
    if args.build_wheelhouse:
        build_wheelhouse(args, sources)
    elif args.install:
        install(args, sources)
    else:
        print(json.dumps({"platform": sources["platform"], "python": sources["python"], "tools": args.tools, "sources": sources, "local_installation": "Offline wheels only, separate venv per tool; LaZagne remains a target resource.", "penelope": "Fixed official MCP commit; owned data-directory adapter avoids changing HOME."}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.CalledProcessError, ValueError, KeyError) as error:
        print("ERROR:", error, file=sys.stderr)
        raise SystemExit(1)
