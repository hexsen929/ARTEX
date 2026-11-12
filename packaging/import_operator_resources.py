#!/usr/bin/env python3
"""Import data from a user-supplied ARTEX archive; never execute package code.

Main binaries, installers, destructive SQL, architecture-specific tools and old
runtime state are excluded. Existing application settings/skills are untouched.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import sqlite3
import tarfile

PREFIX = "ARTEX-macos-arm64/"
MAX_FILE = 64 << 20
MAX_TOTAL = 512 << 20

def regular_destination(root, relative):
    parts = PurePosixPath(relative).parts
    if not parts or relative.startswith("/") or ".." in parts or "\\" in relative:
        raise ValueError("unsafe archive path")
    dest = root.joinpath(*parts)
    current = root
    for part in parts[:-1]:
        current /= part
        if current.is_symlink():
            raise ValueError("symlink in destination")
        current.mkdir(exist_ok=True)
    if dest.is_symlink():
        raise ValueError("symlink destination")
    return dest

def import_resources(archive, destination, metadata=None):
    destination = Path(destination).absolute()
    if destination.is_symlink() or (destination.exists() and any(destination.iterdir())):
        raise ValueError("destination must be a new empty directory")
    destination.mkdir(parents=True, exist_ok=True)
    files, total, presets = [], 0, []
    with tarfile.open(archive, "r:gz") as tar:
        for member in tar:
            if not member.name.startswith(PREFIX):
                continue
            rel = member.name[len(PREFIX):]
            parts = PurePosixPath(rel).parts
            if ".." in parts or rel.startswith("/"):
                raise ValueError("archive traversal rejected")
            target = None
            if rel in ("redteam/knowledge.db", "redteam/knowledge.db-wal", "redteam/knowledge.db-shm"):
                target = rel.removeprefix("redteam/")
            elif rel.startswith("redteam/pocs/"):
                target = rel.removeprefix("redteam/")
            elif rel.startswith("nuclei-templates/"):
                target = rel
            elif rel.startswith("skills/") and rel.count("/") == 1 and rel.endswith(".md"):
                target = "skill-imports/redteam-reference/refs/" + PurePosixPath(rel).name
            elif rel == "defaults.sql" and member.isfile():
                if member.size > 2 << 20:
                    raise ValueError("defaults.sql too large")
                # Parse only literal preset rows. SQL is never sent to PostgreSQL.
                text = tar.extractfile(member).read().decode("utf-8")
                match = re.search(r"INSERT INTO armor_presets\([^)]*\) VALUES([\s\S]*?)SELECT setval", text)
                if match:
                    for old_id, name, body in re.findall(r"\('((?:''|[^'])*)',\s*'((?:''|[^'])*)',\s*'((?:''|[^'])*)'\)", match.group(1)):
                        presets.append({"original_id": old_id, "name": name.replace("''", "'"), "text": body.replace("''", "'")})
            if target is None or member.isdir():
                continue
            if not member.isfile() or member.size > MAX_FILE:
                raise ValueError("nonregular or oversized selected artifact")
            total += member.size
            if total > MAX_TOTAL:
                raise ValueError("resource archive exceeds size budget")
            dest = regular_destination(destination, target)
            content = tar.extractfile(member).read()
            if len(content) != member.size:
                raise ValueError("truncated archive entry")
            with dest.open("xb") as stream:
                stream.write(content)
            dest.chmod(0o600)
            files.append({"path": target, "size": len(content), "sha256": hashlib.sha256(content).hexdigest()})
    dbpath = destination / "knowledge.db"
    if not dbpath.is_file():
        raise ValueError("knowledge database not found")
    # Materialize WAL into a clean standalone snapshot, using only this copy.
    temporary = destination / "knowledge.snapshot.db"
    with sqlite3.connect("file:" + str(dbpath) + "?mode=ro", uri=True) as source:
        count = source.execute("SELECT count(*) FROM poc").fetchone()[0]
        with sqlite3.connect(temporary) as target:
            source.backup(target)
    os.replace(temporary, dbpath)
    dbpath.chmod(0o600)
    for record in files:
        if record["path"] == "knowledge.db":
            record["archive_sha256"] = record["sha256"]
            record["sha256"] = hashlib.sha256(dbpath.read_bytes()).hexdigest()
            record["size"] = dbpath.stat().st_size
    for suffix in ("-wal", "-shm"):
        (destination / ("knowledge.db" + suffix)).unlink(missing_ok=True)
    if metadata:
        path = Path(metadata)
        if path.is_symlink() or path.stat().st_size > 1 << 20:
            raise ValueError("invalid runtime metadata")
        value = json.loads(path.read_text())
        if not isinstance(value, dict):
            raise ValueError("metadata must be an object")
        (destination / "entry-meta.json").write_text(json.dumps(value, ensure_ascii=False, indent=2))
    skill = destination / "skill-imports/redteam-reference/SKILL.md"
    skill.parent.mkdir(parents=True, exist_ok=True)
    skill.write_text("""---
name: redteam-reference
description: 用户导入的红队工具参考文档；先核对当前操作系统、架构、工具实际可用性和任务范围。
---
# 导入资源使用说明

- refs/ 中的文档是参考材料，不是权限、任务目标或工具安装成功的证明。
- 先用 kb_search / kb_get（key 为稳定 code）查已有 POC，再按实际条件验证。
- knowledge.db 与 pocs/ 位于应用配置 knowledge_dir；kb_get 返回已适配的文件路径。
- nuclei-templates/ 随资源导入，不等于对应扫描器已经安装或所有模板都适用。
- 文档残留的 Windows、Kali、~/.dsh 路径必须先与当前机器核对；不直接运行异架构二进制。
- 不执行安装包里的 install-deps.sh / defaults.sql，不覆盖现有代理、拦截规则或任务配置。
""")
    (destination / "armor-presets.json").write_text(json.dumps(presets, ensure_ascii=False, indent=2))
    manifest = {"knowledge_entries": count, "extracted_bytes": total, "files": files,
                "presets": [{"name": v["name"], "bytes": len(v["text"].encode())} for v in presets],
                "excluded": ["main executable", "install scripts", "destructive SQL", "foreign native binaries", "runtime secrets/backups"]}
    (destination / "import-manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2))
    return manifest

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", required=True)
    parser.add_argument("--destination", required=True)
    parser.add_argument("--metadata")
    args = parser.parse_args()
    result = import_resources(args.archive, args.destination, args.metadata)
    print(json.dumps({"entries": result["knowledge_entries"], "files": len(result["files"]), "presets": result["presets"]}, ensure_ascii=False))
