"""Deployment integrity regressions; run in cloud CI, never probe a target."""
import importlib.util
import io
from pathlib import Path
import stat
import struct
import tarfile
import tempfile
import unittest
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


if __name__ == "__main__":
    unittest.main()
