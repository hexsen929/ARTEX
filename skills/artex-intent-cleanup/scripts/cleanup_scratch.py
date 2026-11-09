#!/usr/bin/env python3
"""Opt-in, exact-file cleanup for an ARTEX intent's disposable scratch directory."""

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import uuid

SCRATCH = ".artex-disposable"
OWNER = ".artex-owner.json"
AUDIT = ".artex-cleanup-audit.jsonl"
MAX_FILES = 512
DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW


def require(condition, message):
    if not condition:
        raise ValueError(message)


def identity(s):
    return [s.st_dev, s.st_ino]


def snapshot(s):
    return [s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns, s.st_nlink]


def valid_name(name):
    return (isinstance(name, str) and name not in ("", ".", "..")
            and not name.startswith(".artex-") and "/" not in name
            and "\\" not in name and not any(ord(c) < 32 for c in name))


def read_json_at(fd, name, limit=262144):
    handle = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
    with os.fdopen(handle, "rb") as stream:
        s = os.fstat(stream.fileno())
        require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1 and s.st_size <= limit,
                "JSON must be a bounded regular file with exactly one link")
        data = stream.read(limit + 1)
        require(len(data) <= limit, "JSON is too large")
        return json.loads(data)


def write_json_at(fd, name, value):
    handle = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     0o600, dir_fd=fd)
    with os.fdopen(handle, "w", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())


def file_info(fd, name):
    require(valid_name(name), "Invalid or reserved scratch filename")
    s = os.stat(name, dir_fd=fd, follow_symlinks=False)
    require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1,
            "Only regular files with exactly one link are eligible")
    return {"name": name, "snapshot": snapshot(s), "bytes": s.st_size,
            "allocated_bytes": s.st_blocks * 512}


def check_writers(paths):
    if not paths:
        return
    executable = shutil.which("lsof")
    require(executable is not None, "lsof is required; nothing was deleted")
    result = subprocess.run([executable, "-nP", "-Fpn", "--", *paths],
                            capture_output=True, timeout=20, check=False)
    require(result.returncode == 1 and not result.stdout and not result.stderr,
            "A file is open or lsof could not establish an idle state; nothing was deleted")


def apply_plan(args, root, root_fd, scratch_fd, owner):
    policy = json.loads((Path(__file__).resolve().parents[1] / "policy.json").read_text())
    require(policy == {"version": 1, "mode": "delete-disposable"},
            "Deletion is disabled by policy; preview only")
    require(args.work_complete and args.writers_stopped,
            "Business completion and stopped writers must both be confirmed")
    plan_path = Path(args.apply_plan)
    require(plan_path.is_absolute() and plan_path.parent == root
            and plan_path.name.startswith(".artex-cleanup-plan-")
            and plan_path.suffix == ".json", "Plan must be in this intent directory")
    plan = read_json_at(root_fd, plan_path.name)
    require(plan.get("owner") == owner and plan.get("run_dir") == str(root),
            "Plan belongs to a different intent or scratch directory")
    entries = plan.get("files")
    require(isinstance(entries, list) and 0 < len(entries) <= MAX_FILES,
            "Plan must contain a bounded exact file list")
    names = [entry["name"] for entry in entries]
    require(len(set(names)) == len(names), "Plan has duplicate names")

    def check_directories():
        require(identity(root.lstat()) == owner["run"]
                and identity(os.stat(SCRATCH, dir_fd=root_fd, follow_symlinks=False)) == owner["scratch"],
                "Directory changed after planning")

    def check_file(entry):
        require(file_info(scratch_fd, entry["name"]) == entry,
                "File changed after planning: " + entry["name"])

    check_directories()
    for entry in entries:
        check_file(entry)
    check_writers([str(root / SCRATCH / name) for name in names])
    audit_fd = os.open(AUDIT, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK,
                       0o600, dir_fd=root_fd)
    deleted = []
    try:
        audit_stat = os.fstat(audit_fd)
        require(stat.S_ISREG(audit_stat.st_mode) and audit_stat.st_nlink == 1,
                "Unsafe audit file")
        with os.fdopen(audit_fd, "a", encoding="utf-8", closefd=False) as audit:
            def record(event):
                audit.write(json.dumps(event, ensure_ascii=False) + "\n")
                audit.flush()
                os.fsync(audit_fd)

            for entry in entries:
                check_directories()
                check_file(entry)
                record({"action": "delete_planned", "plan": plan_path.name, **entry})
                os.unlink(entry["name"], dir_fd=scratch_fd)
                deleted.append(entry)
                record({"action": "deleted", "plan": plan_path.name, **entry})
    finally:
        os.close(audit_fd)
        print(json.dumps({"mode": "apply", "deleted": deleted,
                          "deleted_bytes": sum(e["bytes"] for e in deleted)}, ensure_ascii=False))


def execute(args):
    root = Path(args.run_dir)
    require(root.is_absolute() and root == root.resolve(strict=True),
            "Use the canonical absolute intent path without symlinks or traversal")
    require(re.fullmatch(r"i[1-9][0-9]*", root.name)
            and re.fullmatch(r"[1-9][0-9]*", root.parent.name)
            and root.parent.parent.name == "tasks"
            and root.parent.parent.parent.name == "data",
            "Expected data/tasks/<task-id>/i<intent-id>")
    root_fd = os.open(root, DIRECTORY_FLAGS)
    scratch_fd = None
    try:
        created = False
        if args.init:
            try:
                os.mkdir(SCRATCH, 0o700, dir_fd=root_fd)
                created = True
            except FileExistsError:
                pass
        try:
            scratch_fd = os.open(SCRATCH, DIRECTORY_FLAGS, dir_fd=root_fd)
        except FileNotFoundError:
            require(not args.apply_plan and not args.plan, "Scratch directory has not been initialized")
            print(json.dumps({"mode": "preview", "run_dir": str(root),
                              "initialized": False, "eligible_bytes": 0, "files": []}))
            return
        owner = {"version": 1, "run": identity(os.fstat(root_fd)),
                 "scratch": identity(os.fstat(scratch_fd))}
        if created:
            write_json_at(scratch_fd, OWNER, owner)
        require(read_json_at(scratch_fd, OWNER) == owner,
                "Scratch directory has no valid ownership marker; do not adopt old files")
        if args.apply_plan:
            apply_plan(args, root, root_fd, scratch_fd, owner)
            return
        if args.plan:
            require(args.file and len(args.file) <= MAX_FILES, "--plan requires 1 to 512 exact --file names")
            require(len(set(args.file)) == len(args.file), "Duplicate --file names")
            entries = [file_info(scratch_fd, name) for name in args.file]
            plan_name = ".artex-cleanup-plan-" + uuid.uuid4().hex + ".json"
            write_json_at(root_fd, plan_name, {"owner": owner, "run_dir": str(root), "files": entries})
            print(json.dumps({"mode": "preview", "plan_path": str(root / plan_name),
                              "planned_bytes": sum(e["bytes"] for e in entries), "files": entries},
                             ensure_ascii=False))
            return
        entries, skipped = [], []
        with os.scandir(scratch_fd) as scan:
            for item in scan:
                if item.name == OWNER:
                    continue
                require(len(entries) + len(skipped) < MAX_FILES, "Too many scratch entries; inspect manually")
                try:
                    entries.append(file_info(scratch_fd, item.name))
                except (ValueError, OSError):
                    skipped.append(item.name)
        print(json.dumps({"mode": "preview", "run_dir": str(root), "initialized": True,
                          "eligible_bytes": sum(e["bytes"] for e in entries),
                          "files": entries, "skipped": skipped}, ensure_ascii=False))
    finally:
        if scratch_fd is not None:
            os.close(scratch_fd)
        os.close(root_fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_dir")
    action = parser.add_mutually_exclusive_group()
    action.add_argument("--init", action="store_true")
    action.add_argument("--plan", action="store_true")
    action.add_argument("--apply-plan")
    parser.add_argument("--file", action="append", default=[])
    parser.add_argument("--work-complete", action="store_true")
    parser.add_argument("--writers-stopped", action="store_true")
    args = parser.parse_args()
    try:
        require(not args.file or args.plan, "--file is only valid with --plan")
        require(args.apply_plan or not (args.work_complete or args.writers_stopped),
                "Completion flags are only valid when applying a plan")
        execute(args)
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print(json.dumps({"error": str(error)}, ensure_ascii=False), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
