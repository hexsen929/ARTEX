#!/usr/bin/env python3
"""Prepare fixed official tool releases without compiling or running scanners.

The default is a read-only plan. --install downloads into a temporary directory,
verifies the upstream archive hash, checks executable headers, and writes only
new or previously managed files. --destination names the data/tools directory.
"""
from __future__ import annotations

import argparse
import concurrent.futures
import copy
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import secrets
import shlex
import shutil
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
import zipfile

MAX_ARCHIVE_BYTES = 512 * 1024 * 1024
MAX_TREE_BYTES = 1024 * 1024 * 1024
MAX_MEMBERS = 100000
CHUNK = 1024 * 1024


def digest_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(CHUNK), b""):
            h.update(block)
    return h.hexdigest()


def safe_relative(name: str) -> PurePosixPath:
    if not isinstance(name, str) or "\x00" in name or "\\" in name:
        raise ValueError("invalid relative path")
    p = PurePosixPath(name)
    if p.is_absolute() or not p.parts or ".." in p.parts or ":" in p.parts[0]:
        raise ValueError("unsafe relative path: " + name)
    return p


def refuse_symlinks(path: Path) -> None:
    for p in (path, *path.parents):
        if p.is_symlink():
            raise ValueError("symlink destination is not managed: " + str(p))


def target_path(destination: Path, relative: str) -> Path:
    p = safe_relative(relative)
    if p.parts[0] != "tools" or len(p.parts) < 2:
        raise ValueError("tool path must be relative to data and inside tools/")
    result = destination.joinpath(*p.parts[1:])
    refuse_symlinks(result)
    return result


def executable_platforms(blob: bytes) -> set[str]:
    if blob.startswith(b"\x7fELF") and len(blob) >= 20:
        endian = "<" if blob[5] == 1 else ">"
        machine = struct.unpack_from(endian + "H", blob, 18)[0]
        arch = {62: "amd64", 183: "arm64", 3: "386", 40: "arm"}.get(machine)
        return {"linux/" + arch} if arch else set()
    if blob.startswith(b"MZ") and len(blob) >= 64:
        offset = struct.unpack_from("<I", blob, 60)[0]
        if offset + 6 <= len(blob) and blob[offset:offset + 4] == b"PE\0\0":
            machine = struct.unpack_from("<H", blob, offset + 4)[0]
            arch = {0x8664: "amd64", 0xAA64: "arm64", 0x14C: "386"}.get(machine)
            return {"windows/" + arch} if arch else set()
    if len(blob) < 8:
        return set()
    magic = blob[:4]
    if magic in (b"\xcf\xfa\xed\xfe", b"\xce\xfa\xed\xfe", b"\xfe\xed\xfa\xcf", b"\xfe\xed\xfa\xce"):
        endian = "<" if magic[0] in (0xCF, 0xCE) else ">"
        cpu = struct.unpack_from(endian + "I", blob, 4)[0]
        arch = {0x100000C: "arm64", 0x1000007: "amd64", 7: "386", 12: "arm"}.get(cpu)
        return {"darwin/" + arch} if arch else set()
    if magic == b"\xca\xfe\xba\xbe":
        count = struct.unpack_from(">I", blob, 4)[0]
        out = set()
        for i in range(min(count, 32)):
            if 8 + i * 20 + 4 > len(blob):
                break
            cpu = struct.unpack_from(">I", blob, 8 + i * 20)[0]
            arch = {0x100000C: "arm64", 0x1000007: "amd64"}.get(cpu)
            if arch:
                out.add("darwin/" + arch)
        return out
    return set()


def validate_archive_members(members: list[tuple[str, int, bool, bool]]) -> None:
    if len(members) > MAX_MEMBERS:
        raise ValueError("archive has too many entries")
    total = 0
    seen = set()
    for name, size, regular, directory in members:
        p = safe_relative(name)
        if str(p) in seen:
            raise ValueError("duplicate archive entry: " + name)
        seen.add(str(p))
        if not regular and not directory:
            raise ValueError("archive contains a symlink, hardlink or special file")
        total += size
    if total > MAX_TREE_BYTES:
        raise ValueError("archive uncompressed size exceeds limit")


def archive_files(path: Path, kind: str) -> dict[str, bytes]:
    """Read regular archive members, after validating every member."""
    if kind.startswith("zip"):
        with zipfile.ZipFile(path) as z:
            info = z.infolist()
            members = []
            for x in info:
                mode = (x.external_attr >> 16) & 0xFFFF
                special = stat.S_IFMT(mode)
                regular = not x.is_dir() and special in (0, stat.S_IFREG)
                members.append((x.filename, x.file_size, regular, x.is_dir()))
            validate_archive_members(members)
            return {str(safe_relative(x.filename)): z.read(x) for x in info if not x.is_dir()}
    if kind.startswith("tar"):
        with tarfile.open(path, "r:gz") as t:
            info = t.getmembers()
            validate_archive_members([(x.name, x.size, x.isfile(), x.isdir()) for x in info])
            return {str(safe_relative(x.name)): t.extractfile(x).read() for x in info if x.isfile()}
    raise ValueError("unsupported archive type: " + kind)


def extract_payload(path: Path, spec: dict) -> bytes | dict[str, bytes]:
    kind = spec["format"]
    if kind == "raw":
        return path.read_bytes()
    if kind == "gz":
        with gzip.open(path, "rb") as stream:
            blob = stream.read(MAX_ARCHIVE_BYTES + 1)
        if len(blob) > MAX_ARCHIVE_BYTES:
            raise ValueError("decompressed file exceeds limit")
        return blob
    files = archive_files(path, kind)
    if kind.endswith("-tree"):
        prefix = str(safe_relative(spec["strip_prefix"])) + "/" if spec.get("strip_prefix") else ""
        selected = {name[len(prefix):]: blob for name, blob in files.items() if name.startswith(prefix)}
        if not selected:
            raise ValueError("archive contains no files under the expected prefix")
        return selected
    member = spec["member"]
    if "/" in member:
        selected = [blob for name, blob in files.items() if name == member]
    else:
        selected = [blob for name, blob in files.items() if PurePosixPath(name).name == member]
    if len(selected) != 1:
        raise ValueError("archive member is missing or ambiguous: " + member)
    return selected[0]


def download_verified(spec: dict, output: Path) -> None:
    expected = spec["sha256"]
    size = spec["size"]
    if not re.fullmatch(r"[0-9a-f]{64}", expected) or not 0 < size <= MAX_ARCHIVE_BYTES:
        raise ValueError("invalid pinned download metadata")
    if not spec["url"].startswith("https://"):
        raise ValueError("downloads require HTTPS")
    headers = {"User-Agent": "ARTEX-prepare-tools/1", "Accept-Encoding": "identity", "Range": "bytes=0-0"}
    with urllib.request.urlopen(urllib.request.Request(spec["url"], headers=headers), timeout=30) as response:
        final_url = response.geturl()
        if response.status == 200:
            with output.open("wb") as stream:
                total = 0
                while block := response.read(CHUNK):
                    total += len(block)
                    if total > size:
                        raise ValueError("download larger than pinned size")
                    stream.write(block)
            if total != size:
                raise ValueError("download size mismatch")
        else:
            if response.status != 206 or response.headers.get("Content-Range") != f"bytes 0-0/{size}":
                raise ValueError("unexpected download range response")
            response.read(1)
            def part(bounds: tuple[int, int]) -> tuple[int, bytes]:
                start, end = bounds
                last = None
                for _ in range(3):
                    try:
                        req = urllib.request.Request(final_url, headers={"User-Agent": "ARTEX-prepare-tools/1", "Range": f"bytes={start}-{end}", "Accept-Encoding": "identity"})
                        with urllib.request.urlopen(req, timeout=30) as r:
                            if r.status != 206 or r.headers.get("Content-Range") != f"bytes {start}-{end}/{size}":
                                raise ValueError("download range was not honored")
                            blob = r.read(end - start + 2)
                        if len(blob) != end - start + 1:
                            raise ValueError("truncated download range")
                        return start, blob
                    except Exception as exc:
                        last = exc
                raise RuntimeError("fixed release download failed; retry the preparation") from last
            ranges = [(x, min(x + CHUNK - 1, size - 1)) for x in range(0, size, CHUNK)]
            with output.open("wb") as stream:
                stream.truncate(size)
                with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                    for start, blob in pool.map(part, ranges):
                        stream.seek(start)
                        stream.write(blob)
    if digest_file(output) != expected:
        raise ValueError("download SHA256 mismatch")


def atomic_bytes(path: Path, blob: bytes, mode: int = 0o644) -> None:
    refuse_symlinks(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix="." + path.name + "-prepare-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(blob)
            stream.flush()
            os.fchmod(stream.fileno(), mode)
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.lexists(temporary):
            os.unlink(temporary)


def tree_digest(files: dict[str, bytes]) -> str:
    h = hashlib.sha256()
    for name in sorted(files):
        safe_relative(name)
        h.update(name.encode() + b"\0" + hashlib.sha256(files[name]).hexdigest().encode() + b"\n")
    return h.hexdigest()


def install_payload(path: Path, payload: bytes | dict[str, bytes], old: dict | None, expected_platform: str, execution: str) -> dict:
    refuse_symlinks(path)
    if isinstance(payload, bytes):
        platforms = executable_platforms(payload)
        if expected_platform not in ("", "any") and platforms and expected_platform not in platforms:
            raise ValueError(f"wrong executable platform at {path.name}: {sorted(platforms)}")
        if execution == "host" and not platforms:
            raise ValueError("native host tool has no recognized executable header")
        digest = hashlib.sha256(payload).hexdigest()
        if path.exists():
            if not path.is_file():
                raise ValueError("existing path is not a regular file: " + str(path))
            current = digest_file(path)
            if current == digest:
                return {"sha256": digest, "actual_platforms": sorted(platforms)}
            if not old or old.get("sha256") != current:
                raise ValueError("existing custom file would be overwritten: " + str(path))
        atomic_bytes(path, payload, 0o755)
        return {"sha256": digest, "actual_platforms": sorted(platforms)}
    digest = tree_digest(payload)
    if path.exists():
        if not path.is_dir():
            raise ValueError("existing resource path is not a directory")
        actual = {}
        for item in path.rglob("*"):
            refuse_symlinks(item)
            if item.is_file():
                actual[item.relative_to(path).as_posix()] = item.read_bytes()
        if tree_digest(actual) == digest:
            return {"sha256": "", "tree_sha256": digest, "files": len(payload)}
        raise ValueError("existing resource directory differs; preserve it and choose a new destination")
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".tool-tree-", dir=path.parent) as temporary:
        tree = Path(temporary) / "tree"
        tree.mkdir()
        for name, blob in payload.items():
            target = tree.joinpath(*safe_relative(name).parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(blob)
            target.chmod(0o644)
        os.replace(tree, path)
    return {"sha256": "", "tree_sha256": digest, "files": len(payload)}


def host_platform() -> str:
    os_name = {"Darwin": "darwin", "Linux": "linux", "Windows": "windows"}.get(platform.system(), platform.system().lower())
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine(), platform.machine())
    return os_name + "/" + arch


def python_entry_state(path: Path) -> dict:
    """Capture only link text or small pip launchers; never copy runtimes."""
    try:
        st = path.lstat()
    except FileNotFoundError:
        return {"kind": "absent"}
    if stat.S_ISLNK(st.st_mode):
        return {"kind": "symlink", "target": os.readlink(path)}
    if stat.S_ISREG(st.st_mode) and path.name in ("pip", "pip3") and st.st_size <= 65536:
        blob = path.read_bytes()
        if b"\0" not in blob:
            return {"kind": "file", "blob": blob, "mode": stat.S_IMODE(st.st_mode), "mtime_ns": st.st_mtime_ns, "atime_ns": st.st_atime_ns}
    raise ValueError("unmanaged Python entry type; preserve before installing Homebrew dependencies: " + str(path))


def same_python_entry(a: dict, b: dict) -> bool:
    if a["kind"] != b["kind"]:
        return False
    if a["kind"] == "symlink":
        return a["target"] == b["target"]
    if a["kind"] == "file":
        return a["blob"] == b["blob"] and a["mode"] == b["mode"]
    return True


def snapshot_homebrew_python(prefix: Path, cache: Path) -> dict:
    """Keep existing global Python selection in memory, including absent aliases.

    Homebrew's overwrite backup has a fixed path and may replace a user's old
    backup. Reject that conflict before running brew rather than touching it.
    Version-specific python3.X paths and dependency kegs remain unmodified.
    """
    prefix, cache = prefix.resolve(), cache.resolve()
    backup = cache / "Backup"
    if backup.is_symlink():
        raise ValueError("Homebrew Backup is a symlink; leave existing backups untouched")
    names = ("python", "python3", "pythonw", "pythonw3", "python-config", "python3-config", "python3-intel64", "pip", "pip3", "idle", "idle3", "pydoc", "pydoc3", "2to3")
    relative = ["bin/" + name for name in names]
    relative += ["share/man/man1/" + name + suffix for name in names for suffix in (".1", ".1.gz")]
    relative += ["lib/pkgconfig/" + name for name in ("python.pc", "python3.pc", "python-embed.pc", "python3-embed.pc")]
    relative += ["opt/python", "opt/python3", "include/python3", "lib/libpython.dylib", "lib/libpython3.dylib"]
    framework = prefix / "Frameworks/Python.framework"
    if framework.is_symlink():
        relative.append("Frameworks/Python.framework")
    else:
        relative += ["Frameworks/Python.framework/" + name for name in ("Python", "Headers", "Resources", "Versions/Current")]
    states, parents, existing_backups = {}, {}, set()
    for name in relative:
        path = prefix / name
        # An outer alias is restored as one entry; never write through it.
        if any(p.is_symlink() for p in path.parents if p != prefix and prefix in p.parents):
            continue
        state = python_entry_state(path)
        old_backup = backup / name
        if os.path.lexists(old_backup):
            existing_backups.add(old_backup)
        if state["kind"] != "absent" and os.path.lexists(old_backup):
            raise ValueError("existing Homebrew backup would be overwritten; preserve it: " + str(old_backup))
        states[path] = state
        for parent in path.parents:
            if parent == prefix.parent:
                break
            if parent.is_dir() and not parent.is_symlink():
                st = parent.stat()
                parents[parent] = (st.st_dev, st.st_ino)
    existing_dirs = set()
    if backup.exists():
        existing_dirs.add(backup)
        for base, dirs, _ in os.walk(backup, followlinks=False):
            existing_dirs.update(Path(base) / name for name in dirs)
    return {"prefix": prefix, "backup": backup, "states": states, "parents": parents, "existing_backup_dirs": existing_dirs, "existing_backups": existing_backups}


def is_homebrew_python_entry(path: Path, state: dict, prefix: Path) -> bool:
    if state["kind"] == "symlink":
        target = Path(os.path.normpath(path.parent / state["target"]))
        try:
            rel = target.relative_to(prefix)
        except ValueError:
            return False
        return bool(re.match(r"(?:Cellar|opt)/python(?:@\d+(?:\.\d+)*)?(?:/|$)", rel.as_posix())) or rel.as_posix().startswith("Frameworks/Python.framework/")
    if state["kind"] == "file":
        first = state["blob"].split(b"\n", 1)[0]
        return str(prefix).encode() in first and b"python" in first
    return state["kind"] == "absent"


def restore_homebrew_python(snapshot: dict) -> int:
    """Restore only entries that brew replaced; discard only this run's backups."""
    prefix, backup = snapshot["prefix"], snapshot["backup"]
    restored, new_backup_dirs = 0, set()
    for path, before in snapshot["states"].items():
        for parent in path.parents:
            if parent == prefix.parent:
                break
            expected = snapshot["parents"].get(parent)
            if parent.is_symlink() or (expected and (parent.stat().st_dev, parent.stat().st_ino) != expected):
                raise ValueError("Python entry parent changed during Homebrew installation: " + str(parent))
        current = python_entry_state(path)
        if not same_python_entry(before, current):
            if not is_homebrew_python_entry(path, current, prefix):
                raise ValueError("Python entry changed outside this Homebrew installation; preserve it: " + str(path))
            if before["kind"] == "absent":
                path.unlink()
            else:
                path.parent.mkdir(parents=True, exist_ok=True)
                temporary = path.parent / ("." + path.name + ".artex-python-" + secrets.token_hex(12))
                try:
                    if before["kind"] == "symlink":
                        os.symlink(before["target"], temporary)
                    else:
                        with temporary.open("xb") as stream:
                            stream.write(before["blob"])
                            stream.flush()
                            os.fchmod(stream.fileno(), before["mode"])
                            os.fsync(stream.fileno())
                        os.utime(temporary, ns=(before["atime_ns"], before["mtime_ns"]))
                    os.replace(temporary, path)
                finally:
                    if os.path.lexists(temporary):
                        temporary.unlink()
            restored += 1
        if not same_python_entry(before, python_entry_state(path)):
            raise ValueError("original Python entry was not restored: " + str(path))
        item = backup / path.relative_to(prefix)
        if os.path.lexists(item) and item not in snapshot["existing_backups"]:
            # Matching pre-existing backups were rejected before brew. A newly
            # created backup is removed only after its original entry is back.
            if before["kind"] == "absent" or not same_python_entry(before, python_entry_state(item)):
                raise ValueError("unexpected Homebrew backup contents; preserve it: " + str(item))
            item.unlink()
            for parent in item.parents:
                if parent == backup.parent:
                    break
                if parent not in snapshot["existing_backup_dirs"]:
                    new_backup_dirs.add(parent)
    for directory in sorted(new_backup_dirs, key=lambda p: len(p.parts), reverse=True):
        try:
            directory.rmdir()
        except OSError:
            pass  # A prior or unrelated backup must remain in place.
    return restored


def make_plan(manifest: dict, selected_platform: str, names: set[str] | None, host_only: bool) -> tuple[list[dict], list[dict]]:
    plan, skipped = [], []
    known = {entry["name"] for entry in manifest["tools"]}
    if names and names - known:
        raise ValueError("unknown tools: " + ", ".join(sorted(names - known)))
    for entry in manifest["tools"]:
        if names and entry["name"] not in names:
            continue
        execution = entry.get("execution", "host")
        if host_only and execution == "target":
            continue
        choices = entry.get("resources") or entry.get("variants") or [entry]
        added = False
        for choice in choices:
            p = choice.get("platform", entry.get("platform", "any"))
            if execution == "host" and p not in ("any", selected_platform):
                continue
            item = copy.deepcopy(entry)
            item.pop("variants", None)
            item.pop("resources", None)
            item.update(choice)
            if not item.get("download") and not item.get("installer"):
                continue
            if execution == "target" and choice is not entry and "name" not in choice:
                item["name"] = entry["name"] + "-" + p.replace("/", "-")
            item["execution"] = execution
            item["parent_name"] = entry["name"]
            plan.append(item)
            added = True
        if not added:
            skipped.append({"name": entry["name"], "reason": "no fixed native download for this host; use the separate Python preparation or matching resource package"})
    return plan, skipped


def install_homebrew(item: dict, path: Path, allow: bool) -> tuple[dict, dict]:
    if not allow:
        raise ValueError("nmap requires --allow-homebrew for precompiled bottle installation")
    if not host_platform().startswith("darwin/"):
        raise ValueError("Homebrew preparation requires macOS")
    brew = shutil.which("brew")
    if not brew:
        raise ValueError("Homebrew is absent; install its official runtime first")
    env = {**os.environ, "HOMEBREW_NO_AUTO_UPDATE": "1", "HOMEBREW_NO_ANALYTICS": "1", "HOMEBREW_NO_INSTALL_CLEANUP": "1", "HOMEBREW_NO_BOTTLE_SOURCE_FALLBACK": "1"}
    env.pop("HOMEBREW_BUILD_FROM_SOURCE", None)
    formula = item["installer"]["formula"]
    if formula != "nmap":
        raise ValueError("unexpected Homebrew formula in tool catalog")
    info = json.loads(subprocess.check_output([brew, "info", "--json=v2", formula], env=env, text=True))["formulae"][0]
    expected = item["installer"]["version"]
    if info["versions"]["stable"] != expected:
        raise ValueError("Homebrew catalog changed; update the pinned tool catalog before installation")
    installed = info.get("installed", [])
    if installed and installed[-1]["version"].split("_")[0] != expected:
        raise ValueError("existing Homebrew nmap has another version; preserve it and choose an explicit upgrade")
    if not installed:
        if not info.get("bottle", {}).get("stable", {}).get("files"):
            raise ValueError("Homebrew nmap has no precompiled bottle")
        brew_prefix = Path(subprocess.check_output([brew, "--prefix"], env=env, text=True).strip())
        brew_cache = Path(subprocess.check_output([brew, "--cache"], env=env, text=True).strip())
        python_selection = snapshot_homebrew_python(brew_prefix, brew_cache)
        try:
            subprocess.run([brew, "install", "--force-bottle", formula], env=env, check=True)
        finally:
            count = restore_homebrew_python(python_selection)
            if count:
                print(f"Preserved {count} pre-existing global Python entries after Homebrew dependency installation", flush=True)
    info = json.loads(subprocess.check_output([brew, "info", "--json=v2", formula], env=env, text=True))["formulae"][0]
    installed = info.get("installed", [])
    if not installed or installed[-1]["version"].split("_")[0] != item["installer"]["version"]:
        raise ValueError("Homebrew installed version differs from the fixed catalog")
    prefix = Path(subprocess.check_output([brew, "--prefix", formula], env=env, text=True).strip())
    receipt_path = prefix.resolve() / "INSTALL_RECEIPT.json"
    receipt = json.loads(receipt_path.read_text())
    if not receipt.get("poured_from_bottle") or receipt.get("source", {}).get("tap") != "homebrew/core":
        raise ValueError("Homebrew nmap receipt is not an official precompiled bottle")
    binary = (prefix / "bin" / "nmap").resolve()
    with binary.open("rb") as stream:
        platforms = executable_platforms(stream.read(8192))
    if host_platform() not in platforms:
        raise ValueError("Homebrew nmap executable does not match this host")
    wrapper = ("#!/bin/sh\nexport NMAPDIR=" + shlex.quote(str(prefix / "share" / "nmap")) + "\nexec " + shlex.quote(str(binary)) + ' "$@"\n').encode()
    if path.exists() and path.read_bytes() != wrapper:
        raise ValueError("existing nmap wrapper differs; preserve it")
    atomic_bytes(path, wrapper, 0o755)
    resolved = {"resolved_executable": str(binary), "resolved_sha256": digest_file(binary), "runtime_origin": "homebrew-bottle", "homebrew_receipt_path": str(receipt_path), "homebrew_receipt_sha256": digest_file(receipt_path)}
    return {"sha256": digest_file(path), **resolved}, {"source_url": "https://formulae.brew.sh/formula/nmap", "installer": "homebrew-bottle", "receipt": installed[-1]}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--catalog", type=Path, default=Path(__file__).with_name("tools-manifest.json"))
    parser.add_argument("--platform", default=host_platform())
    parser.add_argument("--destination", type=Path, required=True, help="data/tools directory")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--plan", action="store_true", help="read-only plan (default)")
    mode.add_argument("--install", action="store_true")
    parser.add_argument("--tools", help="comma-separated catalog names, e.g. suo5,chisel,httpx")
    parser.add_argument("--host-only", action="store_true", help="omit target-side programs")
    parser.add_argument("--allow-homebrew", action="store_true", help="permit nmap bottle/runtime dependencies; source fallback is disabled")
    parser.add_argument("--manifest-output", type=Path, help="additional explicitly selected local manifest path")
    args = parser.parse_args(argv)
    destination = args.destination.expanduser().absolute()
    refuse_symlinks(destination)
    manifest = json.loads(args.catalog.read_text())
    names = set(args.tools.split(",")) if args.tools else None
    plan, skipped = make_plan(manifest, args.platform, names, args.host_only)
    for item in plan:
        target_path(destination, item["path"])
    if not args.install:
        print(json.dumps({"mode": "plan", "platform": args.platform, "destination": str(destination), "tools": [{k: x[k] for k in ("name", "execution", "platform", "version", "path") if k in x} for x in plan], "skipped": skipped}, ensure_ascii=False, indent=2))
        return 0
    if args.platform != host_platform() and any(x.get("installer") for x in plan):
        raise ValueError("host package-manager preparation requires the actual host platform")
    destination.mkdir(parents=True, exist_ok=True)
    lock_path = destination / "tools-lock.json"
    old_lock = json.loads(lock_path.read_text()) if lock_path.exists() else {"records": []}
    old_records = {x["path"]: x for x in old_lock.get("records", [])}
    records = dict(old_records)
    entries = {}
    local_path = destination / "tools-manifest.local.json"
    if local_path.exists():
        entries = {x["path"]: x for x in json.loads(local_path.read_text()).get("tools", [])}
    for item in plan:
        target = target_path(destination, item["path"])
        print("Preparing " + item["name"] + " (" + item["execution"] + ", " + item.get("platform", "any") + ")", flush=True)
        old = old_records.get(item["path"])
        if item.get("installer"):
            result, source = install_homebrew(item, target, args.allow_homebrew)
        else:
            spec = item["download"]
            if old and target.is_file() and old.get("archive_sha256") == spec["sha256"] and old.get("sha256") and digest_file(target) == old["sha256"]:
                result = {k: old[k] for k in ("sha256", "actual_platforms") if k in old}
            else:
                with tempfile.TemporaryDirectory(prefix=".tool-download-", dir=destination) as temporary:
                    archive = Path(temporary) / "release"
                    download_verified(spec, archive)
                    payload = extract_payload(archive, spec)
                    result = install_payload(target, payload, old, item.get("platform", "any"), item["execution"])
            source = {"source_url": spec["url"], "archive_sha256": spec["sha256"], "archive_bytes": spec["size"]}
        record = {k: item[k] for k in ("name", "execution", "platform", "version", "path") if k in item}
        record.update(source)
        record.update(result)
        records[item["path"]] = record
        entry = {k: item[k] for k in ("name", "purpose", "execution", "platform", "version", "path", "category", "env") if k in item}
        entry.update({k: result[k] for k in ("sha256", "resolved_executable", "resolved_sha256", "runtime_origin", "homebrew_receipt_path", "homebrew_receipt_sha256") if k in result})
        entries[item["path"]] = entry
        # Persist after each successful tool so interrupted downloads are resumable.
        lock = {"schema": 1, "host_platform": args.platform, "records": list(records.values()), "skipped": skipped, "validation": "archive SHA256 and executable header only; no target probes or scanners executed"}
        local = {"_comment": "Locally pinned installed resources. Paths are relative to data, not the tools directory.", "tools": list(entries.values())}
        atomic_bytes(lock_path, (json.dumps(lock, ensure_ascii=False, indent=2) + "\n").encode(), 0o600)
        atomic_bytes(local_path, (json.dumps(local, ensure_ascii=False, indent=2) + "\n").encode())
    if args.manifest_output:
        atomic_bytes(args.manifest_output.expanduser().absolute(), local_path.read_bytes())
    print(json.dumps({"installed_records": len(records), "manifest": str(local_path), "lock": str(lock_path), "skipped": skipped}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, OSError, RuntimeError, subprocess.CalledProcessError) as exc:
        print("prepare-tools: " + str(exc), file=sys.stderr)
        raise SystemExit(1)
