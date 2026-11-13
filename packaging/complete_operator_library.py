#!/usr/bin/env python3
"""Complete a user-supplied operator library without executing its contents.

Preserves every selectable skill (including distinct variants), portable tools,
wordlists and target-platform resources. Runtime credentials/history are not
activated as tool configuration. Host wrappers point at separately verified
native runtimes; foreign executables remain explicit legacy/target artifacts.
"""
from pathlib import Path, PurePosixPath
import argparse
import hashlib
import json
import os
import re
import tarfile
import import_operator_resources as basic

MAX_MEMBER = 256 << 20
MAX_TOOLKIT = 2 << 30
NAME = re.compile(r"^[a-z][a-z0-9._-]{0,119}$")
HOST_TOOLS = {
    "chisel/chisel": "chisel", "gogo/gogo": "gogo", "fscan/fscan": "fscan",
    "httpx/httpx": "httpx", "katana/katana": "katana", "naabu/naabu": "naabu",
    "subfinder/subfinder": "subfinder", "dnsx/dnsx": "dnsx", "ksubdomain/ksubdomain": "ksubdomain",
    "frp/frpc": "frpc", "frp/frps": "frps", "suo5/suo5-linux-amd64": "suo5",
    "suo5/suo5-darwin-arm64": "suo5", "dirsearch/dirsearch": "dirsearch",
    "oneforall/oneforall": "oneforall", "AntSword/antsword": "antsword", "nuclei": "nuclei", "ffuf": "ffuf", "nmap": "nmap",
}


def write(root, relative, raw, mode=0o600):
    path = basic.regular_destination(root, relative)
    if path.exists():
        if not path.is_file() or path.read_bytes() != raw:
            raise ValueError("resource collision: " + relative)
    else:
        with path.open("xb") as stream:
            stream.write(raw)
    path.chmod(mode)
    return path


def metadata(raw):
    text = raw.decode("utf-8-sig").replace("\r\n", "\n")
    head = text.split("\n---", 1)[0] if text.startswith("---\n") else ""
    def field(name):
        match = re.search(r"(?m)^(?:##\s*)?" + name + r":\s*(.+)$", head)
        return match[1].strip().strip("\"'") if match else ""
    return {key: field(key) for key in ("name", "description", "role", "whenToUse")}


def canonical_skill(raw, name, description=None):
    text = raw.decode("utf-8-sig").replace("\r\n", "\n")
    meta = metadata(raw)
    if not NAME.fullmatch(name):
        raise ValueError("invalid skill name")
    # Preserve original valid frontmatter and body unless a distinct variant
    # needs a distinct registry name. Malformed source remains under refs/.
    end = text.find("\n---", 4) if text.startswith("---\n") else -1
    front = text[4:end] if end >= 0 else ""
    malformed = bool(re.search(r"(?m)^(?:#{1,6}\s|```)", front))
    valid_name = bool(re.search(r"(?m)^name:\s*[^\n]+$", front))
    if end >= 0 and valid_name and not malformed and meta["name"] == name and description is None:
        return text.encode()
    if malformed:
        # ScopeSentry puts Markdown before its first horizontal rule; that
        # rule is not a YAML closing delimiter. Preserve the complete guide.
        body = re.sub(r"(?m)^(?:##\s*)?(?:name|description|role|whenToUse):[^\n]*\n?", "", text[4:]).lstrip("\n")
    else:
        body = text[end + 4:].lstrip("\n") if end >= 0 else text
    desc = description or meta["description"] or (name + " imported operator skill")
    header = "---\nname: " + name + "\ndescription: " + json.dumps(desc, ensure_ascii=False) + "\n"
    for key in ("role", "whenToUse"):
        if meta.get(key):
            header += key + ": " + json.dumps(meta[key], ensure_ascii=False) + "\n"
    return (header + "---\n\n" + body).encode()


def excluded_tool_state(relative):
    p = PurePosixPath(relative)
    name = p.name.lower()
    if "__pycache__" in p.parts or "runs" in p.parts or name.endswith((".pyc", ".log", ".out")):
        return "cache_or_run_history"
    if ".bak" in name or name in ("data.db", "data.db-wal", "data.db-shm"):
        return "prior_tool_state_or_backup"
    if name.startswith(("id_rsa", "id_ed25519", "rt-tunnel-key")) or name.endswith((".pem", ".key", ".p12", ".pfx")):
        return "credential_not_tool_capability"
    return None


def host_wrapper(tool, depth=3):
    return ("#!/bin/sh\nset -eu\nHERE=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\n"
            "ROOT=$(CDPATH= cd -- \"$HERE/" + ("../" * depth).rstrip("/") + "\" && pwd)\n"
            "TOOL=\"${ARTEX_DATA_DIR:-$ROOT/data}/tools/" + tool + "\"\n"
            "[ -x \"$TOOL\" ] || { echo 'Missing verified host runtime: " + tool + "' >&2; exit 127; }\n"
            "exec \"$TOOL\" \"$@\"\n").encode()


def complete_library(archive, destination, runtime_metadata=None):
    root = Path(destination).absolute()
    base = basic.import_resources(archive, root, runtime_metadata)
    skill_files, flat, redteam, toolkit = {}, {}, {}, []
    total = 0
    with tarfile.open(archive, "r:gz") as tar:
        for member in tar:
            if not member.name.startswith(basic.PREFIX):
                continue
            rel = member.name[len(basic.PREFIX):]
            p = PurePosixPath(rel)
            if p.is_absolute() or ".." in p.parts or "\\" in rel:
                raise ValueError("unsafe archive path")
            selected = rel.startswith(("skills/", "redteam/skills/", "redteam/toolkit/"))
            if not selected or member.isdir():
                continue
            if not member.isfile() or member.size > MAX_MEMBER:
                raise ValueError("nonregular or oversized selected resource")
            if rel.startswith("redteam/toolkit/"):
                target = rel.removeprefix("redteam/toolkit/")
                reason = excluded_tool_state(target)
                if reason:
                    toolkit.append({"source": rel, "status": "excluded_inactive_state", "reason": reason})
                    continue
                total += member.size
                if total > MAX_TOOLKIT:
                    raise ValueError("toolkit exceeds size budget")
                raw = tar.extractfile(member).read()
                write(root, "toolkit-legacy/" + target, raw)
                # Java/resources and source files are portable; native images
                # stay in the legacy tree until a platform-specific adapter is selected.
                native = raw.startswith((b"\x7fELF", b"MZ", b"\xcf\xfa\xed\xfe", b"\xfe\xed\xfa\xcf"))
                portable = not native and not target.startswith(("jdk11/", "AntSword/"))
                status = "target_or_legacy_binary" if native else "legacy_runtime" if not portable else "portable_resource"
                if portable:
                    write(root, "toolkit/" + target, raw)
                toolkit.append({"source": rel, "path": "toolkit-legacy/" + target,
                                "status": status, "size": len(raw), "sha256": hashlib.sha256(raw).hexdigest()})
            else:
                if member.size > 8 << 20:
                    raise ValueError("skill file too large")
                raw = tar.extractfile(member).read()
                if rel.startswith("redteam/skills/") and rel.count("/") == 2 and rel.endswith(".md"):
                    redteam[p.stem] = raw
                elif rel.startswith("skills/") and rel.count("/") == 1 and rel.endswith(".md"):
                    flat[p.stem] = raw
                elif rel.startswith("skills/") and len(p.parts) >= 3:
                    skill_files["/".join(p.parts[1:])] = raw
    imported = []
    core = sorted({PurePosixPath(p).parts[0] for p in skill_files if p.endswith("/SKILL.md")})
    for folder in core:
        source = skill_files[folder + "/SKILL.md"]
        meta = metadata(source)
        declared = meta["name"] or folder
        for path, raw in sorted(skill_files.items()):
            if path.startswith(folder + "/") and path != folder + "/SKILL.md":
                write(root, "skill-imports/" + path, raw)
        if folder == "redteam-kit-host-notes":
            content = ("---\nname: redteam-kit-host-notes\ndescription: 原红队技能包的跨平台路径适配；在 macOS 上先核对已验证工具与原Windows参考。\n---\n\n"
                "# ARTEX 主机适配\n\n当前主机是 macOS ARM64，不是原资料所写的 Windows。原资料完整保存在 refs/original-windows-notes.md，规则与工具用法可查阅，路径必须使用当前主机事实。\n\n"
                "- 本机原生扫描器：`$ARTEX_DATA_DIR/tools/`；旧 `~/.dsh/redteam/toolkit` 路径映射到配置 `knowledge_dir/toolkit`。\n"
                "- `toolkit-legacy/` 保存其它平台产物，不能把 ELF/PE 当作本机程序运行；需要投递到目标时显式选平台。\n"
                "- 冰蝎/哥斯拉与插件、字典、POC保留在工具库；启动用主机 Java 包装器，不调用原 Windows JDK。\n"
                "- `kb_search` / `kb_get(key)` / `kb_add` 对应知识检索、全文取用与回填；资产/事实/凭据/会话/流量使用当前 ARTEX 同名平台工具。\n"
                "- 未提供的第三方 MCP 配置、VPS私钥和历史会话不是技能安装内容，不根据旧路径推断已经连接。\n").encode()
            write(root, "skill-imports/" + folder + "/refs/original-windows-notes.md", source)
        else:
            content = canonical_skill(source, declared)
            if content != source.replace(b"\r\n", b"\n"):
                write(root, "skill-imports/" + folder + "/refs/original-SKILL.md", source)
        write(root, "skill-imports/" + folder + "/SKILL.md", content)
        imported.append({"directory": folder, "name": declared, "source": "skills/" + folder + "/SKILL.md",
                         "role": meta["role"], "status": "independent_skill", "source_sha256": hashlib.sha256(source).hexdigest()})
    for name, raw in sorted(flat.items()):
        meta = metadata(raw)
        content = canonical_skill(raw, meta["name"] or name)
        write(root, "skill-imports/" + name + "/SKILL.md", content)
        write(root, "skill-imports/" + name + "/refs/original-root.md", raw)
        imported.append({"directory": name, "name": meta["name"] or name, "source": "skills/" + name + ".md",
                         "role": meta["role"], "status": "independent_skill", "source_sha256": hashlib.sha256(raw).hexdigest()})
        other = redteam.get(name)
        if other and other.replace(b"\r\n", b"\n").strip() != raw.replace(b"\r\n", b"\n").strip():
            variant = name + "-redteam"
            write(root, "skill-imports/" + variant + "/SKILL.md", canonical_skill(other, variant))
            write(root, "skill-imports/" + name + "/refs/redteam-variant.md", other)
            imported.append({"directory": variant, "name": variant, "source": "redteam/skills/" + name + ".md",
                             "role": metadata(other)["role"], "status": "independent_variant", "source_sha256": hashlib.sha256(other).hexdigest()})
    for rel, tool in HOST_TOOLS.items():
        path = root / "toolkit" / rel
        # A source launcher can be present; keep its bytes in toolkit-legacy and
        # replace only this new host adapter tree, never the installation itself.
        if path.exists():
            path.unlink()
        write(root, "toolkit/" + rel, host_wrapper(tool,len(PurePosixPath(rel).parts)+1), 0o700)
    for relative,tool in [("dirsearch/dirsearch.py","dirsearch"),("oneforall/OneForAll-0.4.5/oneforall.py","oneforall")]:
        adapter=("#!/usr/bin/env python3\nimport os,pathlib,sys\n"
                 "root=next(p.parent for p in pathlib.Path(__file__).resolve().parents if p.name=='toolkit')\n"
                 "target=pathlib.Path(os.environ.get('ARTEX_DATA_DIR',str(root.parent/'data')))/'tools'/"+repr(tool)+"\n"
                 "if not target.is_file():raise SystemExit('Missing verified host runtime: "+tool+"')\n"
                 "os.execv(str(target),[str(target),*sys.argv[1:]])\n").encode()
        path=root/'toolkit'/relative
        if path.exists():path.unlink()
        write(root,'toolkit/'+relative,adapter,0o700)
    java = b'''#!/bin/sh
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
JAVA="${ARTEX_DATA_DIR:-$ROOT/data}/tools/java/bin/java"
[ -x "$JAVA" ] || { echo 'Missing verified Java runtime' >&2; exit 127; }
exec "$JAVA" "$@"
'''
    write(root, "toolkit/java", java, 0o700)
    for name, jar in [("behinder", "Behinder_v4.1/Behinder.jar"), ("godzilla-cli", "Godzilla/godzilla.jar")]:
        script = ('#!/bin/sh\nset -eu\nHERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\n'
                  'exec "$HERE/java" -jar "$HERE/' + jar + '" "$@"\n').encode()
        write(root, "toolkit/" + name, script, 0o700)
    # Only the Behinder launcher needs JavaFX modules. Godzilla uses the
    # ordinary verified JVM; both retain their original JAR/plugin contents.
    behinder=root/'toolkit/behinder'
    behinder.write_text('#!/bin/sh\nset -eu\nHERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\nROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)\nexec "${ARTEX_DATA_DIR:-$ROOT/data}/tools/javafx-java" -jar "$HERE/Behinder_v4.1/Behinder.jar" "$@"\n')
    behinder.chmod(0o700)
    result = {"schema": 2, "independent_skills": imported, "toolkit": toolkit,
              "host_adapters": HOST_TOOLS, "standard_skill_count": len(core),
              "flat_skill_count": len(flat), "distinct_variant_count": sum(x["status"] == "independent_variant" for x in imported),
              "toolkit_bytes": total, "requires_verified_host_runtime": True,
              "sensitive_state_is_not_activated": True}
    (root / "complete-library-manifest.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
    return result

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", required=True)
    parser.add_argument("--destination", required=True)
    parser.add_argument("--metadata")
    args = parser.parse_args()
    result = complete_library(args.archive, args.destination, args.metadata)
    print(json.dumps({"skills": len(result["independent_skills"]), "core": result["standard_skill_count"],
                      "flat": result["flat_skill_count"], "variants": result["distinct_variant_count"],
                      "toolkit_files": len(result["toolkit"]), "toolkit_bytes": result["toolkit_bytes"]}, ensure_ascii=False))
