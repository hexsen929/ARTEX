#!/usr/bin/env python3
"""Register verified host tools as ARTEX Bash environment declarations.

The default is a file-only plan. --apply explicitly inserts missing custom shell
rows; existing rows, agent permissions and approval settings are never updated.
No tool binary is executed by this command. Run it again to register newly
installed tools; it deliberately does not claim runtime/protocol acceptance.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shlex
import shutil
import struct
import subprocess
import sys


class RegistrationError(Exception):
    pass


HOMEBREW_PREFIXES = {
    "darwin/arm64": Path("/opt/homebrew"),
    "darwin/amd64": Path("/usr/local"),
}


def host_platform() -> str:
    system = {"Darwin": "darwin", "Linux": "linux", "Windows": "windows"}.get(
        platform.system(), platform.system().lower()
    )
    machine = platform.machine().lower()
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(
        machine, machine
    )
    return f"{system}/{arch}"


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def executable_platforms(path: Path) -> set[str]:
    """Read headers only; an empty set means a script or unknown format."""
    with path.open("rb") as stream:
        header = stream.read(4096)
        if len(header) < 8:
            return set()
        if header[:4] == b"\x7fELF" and len(header) >= 20:
            endian = "<" if header[5] == 1 else ">"
            arch = {62: "amd64", 183: "arm64", 3: "386", 40: "arm"}.get(
                struct.unpack_from(endian + "H", header, 18)[0]
            )
            return {f"linux/{arch}"} if arch else set()
        if header[:2] == b"MZ" and len(header) >= 64:
            offset = struct.unpack_from("<I", header, 60)[0]
            if offset > 16 * 1024 * 1024:
                return set()
            stream.seek(offset)
            pe = stream.read(6)
            if len(pe) != 6 or pe[:4] != b"PE\x00\x00":
                return set()
            arch = {0x8664: "amd64", 0xAA64: "arm64", 0x14C: "386"}.get(
                struct.unpack_from("<H", pe, 4)[0]
            )
            return {f"windows/{arch}"} if arch else set()
        magic = struct.unpack_from(">I", header)[0]
        cpu_arch = {0x01000007: "amd64", 0x0100000C: "arm64", 7: "386", 12: "arm"}
        if magic in {0xCAFEBABE, 0xCAFEBABF}:
            count = struct.unpack_from(">I", header, 4)[0]
            size = 32 if magic == 0xCAFEBABF else 20
            if count > 64 or 8 + count * size > len(header):
                return set()
            return {
                f"darwin/{cpu_arch[cpu]}"
                for index in range(count)
                if (cpu := struct.unpack_from(">I", header, 8 + index * size)[0]) in cpu_arch
            }
        if magic in {0xCEFAEDFE, 0xCFFAEDFE, 0xFEEDFACE, 0xFEEDFACF}:
            endian = "<" if magic in {0xCEFAEDFE, 0xCFFAEDFE} else ">"
            arch = cpu_arch.get(struct.unpack_from(endian + "I", header, 4)[0])
            return {f"darwin/{arch}"} if arch else set()
    return set()


def read_manifest(path: Path) -> list[dict]:
    if not path.is_file() or path.stat().st_size > 16 * 1024 * 1024:
        raise RegistrationError(f"manifest missing or too large: {path}")
    try:
        content = json.loads(path.read_text(encoding="utf-8"))
    except (ValueError, UnicodeError) as error:
        raise RegistrationError(f"invalid JSON manifest: {path}") from error
    entries = content.get("tools", content.get("records")) if isinstance(content, dict) else None
    if not isinstance(entries, list) or any(not isinstance(entry, dict) for entry in entries):
        raise RegistrationError(f"manifest requires a tools or records array: {path}")
    return entries


def selected_host_entry(entry: dict, host: str) -> dict | None:
    # Require positive host classification. Old target-payload records and all
    # target variants must never become executable declarations on the Mac.
    if entry.get("execution") != "host":
        return None
    variants = entry.get("variants")
    if variants:
        if isinstance(variants, dict):
            variant = variants.get(host)
            variants = [dict(variant, platform=host)] if isinstance(variant, dict) else []
        if not isinstance(variants, list):
            raise RegistrationError("tool variants must be an array")
        matches = [variant for variant in variants if isinstance(variant, dict) and variant.get("platform") == host]
        if len(matches) > 1:
            raise RegistrationError(f"duplicate host variants: {entry.get('name', '')}")
        if not matches:
            return None
        entry = {**entry, **matches[0]}
    if entry.get("platform") not in {host, "any"}:
        return None
    return entry


def verify_homebrew_nmap(entry: dict, command: Path, host: str) -> None:
    """Permit the one separately managed runtime, with pinned bottle receipt.

    A manifest flag alone is insufficient: the resolved executable must have
    the standard Homebrew nmap keg layout and its on-disk receipt must identify
    a poured homebrew/core bottle. Other external runtime paths stay rejected.
    """
    prefix = HOMEBREW_PREFIXES.get(host)
    if entry.get("name") != "nmap" or entry.get("runtime_origin") != "homebrew-bottle" or prefix is None:
        raise RegistrationError("resolved command escaped data/tools")
    try:
        cellar = (prefix / "Cellar").resolve(strict=True)
        relative = command.relative_to(cellar)
    except (OSError, ValueError) as error:
        raise RegistrationError("nmap runtime is outside the standard Homebrew Cellar") from error
    parts = relative.parts
    if len(parts) != 4 or parts[0] != "nmap" or parts[2:] != ("bin", "nmap") or not re.fullmatch(r"[0-9]+(?:\.[0-9]+)*(?:_[0-9]+)?", parts[1]):
        raise RegistrationError("nmap runtime is not a versioned Homebrew nmap executable")
    if entry.get("version") and parts[1].split("_", 1)[0] != str(entry["version"]).lstrip("v"):
        raise RegistrationError("nmap runtime version differs from the installed manifest")
    expected_receipt = command.parent.parent / "INSTALL_RECEIPT.json"
    try:
        raw_receipt = Path(entry.get("homebrew_receipt_path", ""))
        if raw_receipt.is_symlink():
            raise RegistrationError("nmap bottle receipt cannot be a symlink")
        receipt = raw_receipt.resolve(strict=True)
    except (OSError, TypeError) as error:
        raise RegistrationError("nmap bottle receipt is missing") from error
    if receipt != expected_receipt or receipt.is_symlink():
        raise RegistrationError("nmap bottle receipt is outside its installed keg")
    digest = entry.get("homebrew_receipt_sha256", "")
    if not re.fullmatch(r"[0-9a-fA-F]{64}", str(digest)) or file_sha256(receipt) != digest.lower():
        raise RegistrationError("nmap bottle receipt checksum mismatch")
    try:
        content = json.loads(receipt.read_text(encoding="utf-8"))
    except (OSError, ValueError, UnicodeError) as error:
        raise RegistrationError("nmap bottle receipt is invalid") from error
    if not isinstance(content, dict):
        raise RegistrationError("nmap bottle receipt is invalid")
    source = content.get("source", {})
    if content.get("poured_from_bottle") is not True or not isinstance(source, dict) or source.get("tap") != "homebrew/core":
        raise RegistrationError("nmap runtime is not a poured Homebrew core bottle")
    if host not in executable_platforms(command) or not os.access(command, os.X_OK):
        raise RegistrationError("nmap Homebrew executable does not match the host")


def verified_path(entry: dict, data_dir: Path, host: str) -> Path:
    raw = entry.get("path")
    if not isinstance(raw, str):
        raise RegistrationError("tool path must be a data-relative string")
    relative = PurePosixPath(raw)
    if (
        relative.is_absolute()
        or ".." in relative.parts
        or not relative.parts
        or relative.parts[0] != "tools"
        or "\\" in raw
    ):
        raise RegistrationError(f"tool path must stay inside data/tools: {raw}")
    tools_dir = (data_dir / "tools").resolve()
    candidate = data_dir.joinpath(*relative.parts)
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(tools_dir)
    except (OSError, ValueError) as error:
        raise RegistrationError(f"missing tool or path outside data/tools: {raw}") from error
    if not resolved.is_file() or not os.access(candidate, os.X_OK):
        raise RegistrationError(f"host tool is not an executable file: {raw}")
    expected = entry.get("sha256", "")
    if not isinstance(expected, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", expected):
        raise RegistrationError(f"host tool requires a pinned sha256: {raw}")
    if file_sha256(resolved) != expected.lower():
        raise RegistrationError(f"host tool checksum mismatch: {raw}")
    actual = executable_platforms(resolved)
    if actual and host not in actual:
        raise RegistrationError(f"host tool architecture mismatch: {raw}")
    if not actual:
        with resolved.open("rb") as stream:
            if stream.read(2) != b"#!":
                raise RegistrationError(f"host tool has an unrecognized executable format: {raw}")
    # A lock can additionally pin the real venv command behind an installed shim.
    if entry.get("resolved_executable"):
        underlying = Path(entry["resolved_executable"])
        if not underlying.is_absolute():
            underlying = data_dir / underlying
        try:
            underlying = underlying.resolve(strict=True)
        except OSError as error:
            raise RegistrationError(f"resolved command is missing: {raw}") from error
        try:
            underlying.relative_to(tools_dir)
        except ValueError:
            verify_homebrew_nmap(entry, underlying, host)
        digest = entry.get("resolved_sha256", "")
        if not re.fullmatch(r"[0-9a-fA-F]{64}", str(digest)) or file_sha256(underlying) != digest.lower():
            raise RegistrationError(f"resolved command checksum mismatch: {raw}")
    return candidate.absolute()


def registration_rows(manifests: list[Path], data_dir: Path, agents: list[str], host: str) -> tuple[list[dict], int]:
    rows: dict[str, dict] = {}
    skipped = 0
    for manifest in manifests:
        for original in read_manifest(manifest):
            entry = selected_host_entry(original, host)
            if entry is None:
                skipped += 1
                continue
            name = entry.get("name", "")
            if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z][A-Za-z0-9_.-]{0,63}", name):
                raise RegistrationError("invalid tool name")
            executable = verified_path(entry, data_dir, host)
            key = "external_" + re.sub(r"[^a-z0-9_]", "_", name.lower())
            purpose = entry.get("purpose") or "外部命令行工具"
            if not isinstance(purpose, str) or any(ord(character) < 32 for character in purpose):
                raise RegistrationError(f"invalid purpose text: {name}")
            purpose = purpose[:2000]
            description = (
                f"{purpose}。固定调用路径：{shlex.quote(str(executable))}。"
                "通过现有 Bash 调用并遵循其权限与审批；不要按裸命令名猜测 PATH 中的同名程序。"
                "此记录表示文件完整性和本机架构已核对，具体功能以实际调用结果为准。"
            )
            if name == "nuclei":
                templates = (data_dir / "tools" / "nuclei-templates").absolute()
                description += (
                    f" 模板显式指定 -t {shlex.quote(str(templates))}，使用 -duc 关闭自动更新检查；"
                    "不自动下载模板或写入全局模板目录。"
                )
            row = {"key": key, "description": description, "agents": agents, "path": str(executable)}
            previous = rows.get(key)
            if previous and previous["path"] != row["path"]:
                raise RegistrationError(f"conflicting registered paths for {key}")
            if not previous:
                rows[key] = row
    return sorted(rows.values(), key=lambda row: row["key"]), skipped


def connection_environment(config_path: Path) -> dict[str, str]:
    environment = os.environ.copy()
    dsn = environment.get("ARTEX_PG_DSN", "").strip()
    if not dsn:
        try:
            content = json.loads(config_path.read_text(encoding="utf-8"))
        except (OSError, ValueError, UnicodeError) as error:
            raise RegistrationError("database configuration is missing or invalid") from error
        database = content.get("database", {})
        if not isinstance(database, dict):
            raise RegistrationError("database configuration must be an object")
        dsn = str(database.get("dsn", "")).strip()
        if not dsn:
            mapping = {"host": "PGHOST", "port": "PGPORT", "user": "PGUSER", "password": "PGPASSWORD", "dbname": "PGDATABASE", "sslmode": "PGSSLMODE"}
            if not any(database.get(field) for field in ["host", "user", "dbname"]):
                raise RegistrationError("database configuration is empty")
            # Component mode must not inherit an unrelated ambient database name
            # or password when config.json supplies an empty/default component.
            for field, variable in mapping.items():
                value = database.get(field)
                if value not in (None, ""):
                    environment[variable] = str(value)
                else:
                    environment.pop(variable, None)
    if dsn:
        # psql expands libpq conninfo in dbname. Keep credentials in the child
        # environment, never in command arguments, output or a temporary file.
        environment["PGDATABASE"] = dsn
    environment["PGCONNECT_TIMEOUT"] = "10"
    environment["PGAPPNAME"] = "artex-register-tools"
    return environment


def insert_sql(rows: list[dict]) -> str:
    # Hex contains no SQL delimiters, quotes or backslashes. Parameters are sent
    # on stdin to psql, rather than interpolated as SQL strings or shell code.
    payload = json.dumps(rows, ensure_ascii=False).encode("utf-8").hex()
    return f"""BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '20s';
WITH proposed AS (
 SELECT * FROM jsonb_to_recordset(convert_from(decode('{payload}', 'hex'), 'UTF8')::jsonb)
 AS row(key text, description text, agents jsonb, path text)
), inserted AS (
 INSERT INTO tools(key,system,description,schema,agents,enabled,kind,exec,deferred)
 SELECT key,false,description,'{{}}'::jsonb,agents,true,'shell','{{}}'::jsonb,false FROM proposed
 ON CONFLICT (key) DO NOTHING
 RETURNING key
)
SELECT jsonb_build_object(
 'inserted_keys',(SELECT coalesce(jsonb_agg(key ORDER BY key),'[]'::jsonb) FROM inserted),
 'kept_existing',(SELECT count(*) FROM proposed)-(SELECT count(*) FROM inserted),
 'existing_rows_and_approval_settings_unchanged',true);
COMMIT;
"""


def apply_rows(rows: list[dict], config_path: Path, psql: str) -> dict:
    environment = connection_environment(config_path)
    try:
        result = subprocess.run(
            [psql, "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1"],
            input=insert_sql(rows), text=True, capture_output=True, env=environment, timeout=40,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RegistrationError("database registration did not complete") from error
    if result.returncode:
        # libpq diagnostics may include a user-supplied connection string.
        raise RegistrationError(f"database registration failed (psql exit {result.returncode}); no credentials printed")
    try:
        return json.loads(result.stdout.strip())
    except ValueError as error:
        raise RegistrationError("database registration returned an unexpected result") from error


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data", type=Path, required=True, help="ARTEX data directory")
    parser.add_argument("--manifest", type=Path, action="append", help="installed local manifest or lock; repeat to merge")
    parser.add_argument("--config", type=Path, help="config.json path (default: ARTEX_CONFIG or data parent/config.json)")
    parser.add_argument("--agent", action="append", help="new declaration binding (default: worker); existing bindings stay intact")
    operation = parser.add_mutually_exclusive_group()
    operation.add_argument("--plan", action="store_true", help="file-only plan, no database connection (default)")
    operation.add_argument("--apply", action="store_true", help="insert missing custom shell declarations")
    args = parser.parse_args(argv)
    try:
        data_dir = args.data.absolute()
        agents = list(dict.fromkeys(args.agent or ["worker"]))
        if any(not re.fullmatch(r"[a-z][a-z0-9_]{0,63}", agent) for agent in agents):
            raise RegistrationError("invalid agent key")
        manifests = args.manifest or [
            path for name in ["tools-manifest.local.json", "python-tools-manifest.local.json"]
            if (path := data_dir / "tools" / name).is_file()
        ]
        if not manifests:
            raise RegistrationError("no installed local manifests found; install tools first or specify --manifest")
        host = host_platform()
        rows, skipped = registration_rows(manifests, data_dir, agents, host)
        report = {"mode": "apply" if args.apply else "plan", "host_platform": host, "host_tools": len(rows), "target_or_other_platform_records_skipped": skipped, "declarations": [{"key": row["key"], "path": row["path"]} for row in rows], "tool_binaries_executed": False}
        if args.apply:
            psql = shutil.which("psql")
            if not psql:
                raise RegistrationError("psql is required for --apply")
            config_path = args.config or Path(os.environ.get("ARTEX_CONFIG") or data_dir.parent / "config.json")
            report["database"] = apply_rows(rows, config_path, psql)
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return 0
    except (RegistrationError, OSError) as error:
        print(f"register-tools: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
