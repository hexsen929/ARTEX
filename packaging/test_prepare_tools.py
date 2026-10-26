"""Deployment integrity regressions; run in cloud CI, never probe a target."""
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import struct
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock
import zipfile


SPEC = importlib.util.spec_from_file_location("prepare_tools", Path(__file__).parent / "prepare-tools.py")
tools = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(tools)


def macho_arm64(extra=b""):
    return struct.pack("<II", 0xFEEDFACF, 0x100000C) + extra


class ArchiveIntegrityTests(unittest.TestCase):
    def test_rejects_traversal_before_selecting_a_valid_member(self):
        with tempfile.TemporaryDirectory() as temporary:
            archive = Path(temporary) / "release.zip"
            with zipfile.ZipFile(archive, "w") as z:
                z.writestr("httpx", macho_arm64())
                z.writestr("../outside", b"bad")
            with self.assertRaisesRegex(ValueError, "unsafe relative"):
                tools.extract_payload(archive, {"format": "zip", "member": "httpx"})
            self.assertFalse((Path(temporary).parent / "outside").exists())

    def test_rejects_zip_symlink_even_when_not_the_selected_binary(self):
        with tempfile.TemporaryDirectory() as temporary:
            archive = Path(temporary) / "release.zip"
            with zipfile.ZipFile(archive, "w") as z:
                z.writestr("chisel", macho_arm64())
                link = zipfile.ZipInfo("link")
                link.create_system = 3
                link.external_attr = (stat.S_IFLNK | 0o777) << 16
                z.writestr(link, "../../other")
            with self.assertRaisesRegex(ValueError, "symlink"):
                tools.extract_payload(archive, {"format": "zip", "member": "chisel"})

    def test_rejects_tar_hardlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            archive = Path(temporary) / "release.tar.gz"
            with tarfile.open(archive, "w:gz") as t:
                file = tarfile.TarInfo("ffuf")
                file.size = 4
                t.addfile(file, io.BytesIO(b"test"))
                link = tarfile.TarInfo("link")
                link.type = tarfile.LNKTYPE
                link.linkname = "ffuf"
                t.addfile(link)
            with self.assertRaisesRegex(ValueError, "hardlink"):
                tools.extract_payload(archive, {"format": "tar.gz", "member": "ffuf"})

    def test_selects_only_requested_tree_prefix(self):
        with tempfile.TemporaryDirectory() as temporary:
            archive = Path(temporary) / "source.zip"
            with zipfile.ZipFile(archive, "w") as z:
                z.writestr("project/Mac/main.py", b"print('fixture')")
                z.writestr("project/Windows/main.py", b"other")
            selected = tools.extract_payload(archive, {"format": "zip-tree", "strip_prefix": "project/Mac"})
            self.assertEqual(selected, {"main.py": b"print('fixture')"})


class InstallationTests(unittest.TestCase):
    def test_repeated_install_is_idempotent_and_preserves_custom_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary).resolve() / "httpx"
            payload = macho_arm64(b"official fixture")
            first = tools.install_payload(path, payload, None, "darwin/arm64", "host")
            inode = path.stat().st_ino
            again = tools.install_payload(path, payload, first, "darwin/arm64", "host")
            self.assertEqual(first, again)
            self.assertEqual(inode, path.stat().st_ino)
            custom = macho_arm64(b"user custom")
            path.write_bytes(custom)
            with self.assertRaisesRegex(ValueError, "custom file"):
                tools.install_payload(path, payload, first, "darwin/arm64", "host")
            self.assertEqual(path.read_bytes(), custom)

    def test_host_rejects_wrong_header_before_any_file_write(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary).resolve() / "tool"
            with self.assertRaisesRegex(ValueError, "wrong executable platform"):
                tools.install_payload(path, macho_arm64(), None, "linux/amd64", "host")
            self.assertFalse(path.exists())

    def test_target_and_host_chisel_have_separate_paths(self):
        manifest = {"tools": [
            {"name": "chisel", "execution": "host", "variants": [
                {"platform": "darwin/arm64", "path": "tools/chisel", "download": {"format": "gz"}},
                {"platform": "linux/amd64", "path": "tools/chisel", "download": {"format": "gz"}},
            ]},
            {"name": "chisel-target", "execution": "target", "variants": [
                {"platform": "linux/amd64", "path": "tools/targets/linux-amd64/chisel", "download": {"format": "gz"}},
                {"platform": "windows/amd64", "path": "tools/targets/windows-amd64/chisel.exe", "download": {"format": "zip"}},
            ]},
        ]}
        plan, skipped = tools.make_plan(manifest, "darwin/arm64", None, False)
        self.assertFalse(skipped)
        self.assertEqual(len(plan), 3)
        self.assertEqual(plan[0]["platform"], "darwin/arm64")
        self.assertEqual(len({x["path"] for x in plan}), 3)
        self.assertTrue(all(x["path"].startswith("tools/targets/") for x in plan[1:]))

    def test_tool_paths_cannot_leave_tools_root(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "data" / "tools"
            with self.assertRaises(ValueError):
                tools.target_path(destination, "tools/../config.json")
            with self.assertRaises(ValueError):
                tools.target_path(destination, "jwt.key")


class HomebrewPythonSelectionTests(unittest.TestCase):
    def test_preserves_mixed_python_links_pip_script_and_framework(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            prefix, cache = root / "brew", root / "cache"
            (prefix / "bin").mkdir(parents=True)
            (prefix / "Frameworks/Python.framework/Versions").mkdir(parents=True)
            python = prefix / "bin/python3"
            python.symlink_to("../Cellar/python@3.14/3.14.4/bin/python3")
            pip = prefix / "bin/pip3"
            old_pip = b"#!/fixture/python3.11\n# pre-existing pip launcher\n"
            pip.write_bytes(old_pip)
            pip.chmod(0o755)
            current = prefix / "Frameworks/Python.framework/Versions/Current"
            current.symlink_to("3.14")
            saved = tools.snapshot_homebrew_python(prefix, cache)
            for entry in (python, pip, current):
                backup = cache / "Backup" / entry.relative_to(prefix)
                backup.parent.mkdir(parents=True, exist_ok=True)
                os.replace(entry, backup)
            python.symlink_to("../Cellar/python@3.15/3.15.0/bin/python3")
            pip.write_bytes(("#!" + str(prefix / "opt/python@3.15/bin/python3.15") + "\n# new pip\n").encode())
            current.symlink_to("3.15")
            (prefix / "opt").mkdir()
            new_alias = prefix / "opt/python3"
            new_alias.symlink_to("../Cellar/python@3.15/3.15.0")
            dependency = prefix / "Cellar/python@3.15/3.15.0"
            dependency.mkdir(parents=True)
            self.assertEqual(tools.restore_homebrew_python(saved), 4)
            self.assertEqual(os.readlink(python), "../Cellar/python@3.14/3.14.4/bin/python3")
            self.assertEqual(pip.read_bytes(), old_pip)
            self.assertEqual(stat.S_IMODE(pip.stat().st_mode), 0o755)
            self.assertEqual(os.readlink(current), "3.14")
            self.assertFalse(os.path.lexists(new_alias))
            self.assertTrue(dependency.is_dir())
            self.assertFalse((cache / "Backup").exists())

    def test_existing_conflicting_backup_stops_before_install(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            prefix, cache = root / "brew", root / "cache"
            (prefix / "bin").mkdir(parents=True)
            python = prefix / "bin/python3"
            python.symlink_to("../Cellar/python@3.14/3.14.4/bin/python3")
            prior = cache / "Backup/bin/python3"
            prior.parent.mkdir(parents=True)
            prior.write_bytes(b"user pre-existing backup")
            with self.assertRaisesRegex(ValueError, "existing Homebrew backup"):
                tools.snapshot_homebrew_python(prefix, cache)
            self.assertEqual(prior.read_bytes(), b"user pre-existing backup")
            self.assertEqual(os.readlink(python), "../Cellar/python@3.14/3.14.4/bin/python3")

    def test_unrelated_existing_backup_remains(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            prefix, cache = root / "brew", root / "cache"
            prefix.mkdir()
            prior = cache / "Backup/bin/other-tool"
            prior.parent.mkdir(parents=True)
            prior.write_bytes(b"preserve")
            saved = tools.snapshot_homebrew_python(prefix, cache)
            self.assertEqual(tools.restore_homebrew_python(saved), 0)
            self.assertEqual(prior.read_bytes(), b"preserve")

    def test_failed_brew_install_still_restores_default_python(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            prefix, cache = root / "brew", root / "cache"
            (prefix / "bin").mkdir(parents=True)
            python = prefix / "bin/python3"
            original = "../Cellar/python@3.14/3.14.4/bin/python3"
            python.symlink_to(original)
            info = {"formulae": [{"versions": {"stable": "7.992"}, "installed": [], "bottle": {"stable": {"files": {"arm64_fixture": {}}}}}]}
            def failed_install(*args, **kwargs):
                backup = cache / "Backup/bin/python3"
                backup.parent.mkdir(parents=True)
                os.replace(python, backup)
                python.symlink_to("../Cellar/python@3.15/3.15.0/bin/python3")
                raise subprocess.CalledProcessError(1, "brew fixture")
            item = {"installer": {"formula": "nmap", "version": "7.992"}}
            with mock.patch.object(tools, "host_platform", return_value="darwin/arm64"), mock.patch.object(tools.shutil, "which", return_value=str(prefix / "bin/brew")), mock.patch.object(tools.subprocess, "check_output", side_effect=[json.dumps(info), str(prefix), str(cache)]), mock.patch.object(tools.subprocess, "run", side_effect=failed_install):
                with self.assertRaises(subprocess.CalledProcessError):
                    tools.install_homebrew(item, root / "tools/nmap", True)
            self.assertEqual(os.readlink(python), original)
            self.assertFalse((cache / "Backup").exists())


if __name__ == "__main__":
    unittest.main()
