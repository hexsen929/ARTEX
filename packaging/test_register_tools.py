"""Registration boundary regressions; executed by cloud CI, not during install."""

import importlib.util
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("register_tools", Path(__file__).with_name("register-tools.py"))
register = importlib.util.module_from_spec(spec)
spec.loader.exec_module(register)


class RegistrationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.data = Path(self.temporary.name) / "data"
        (self.data / "tools").mkdir(parents=True)

    def executable(self, relative, platform="darwin/arm64"):
        path = self.data / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        if platform == "darwin/arm64":
            path.write_bytes(struct.pack("<II", 0xFEEDFACF, 0x0100000C) + b"\x00" * 24)
        elif platform == "linux/amd64":
            blob = bytearray(64)
            blob[:6] = b"\x7fELF\x02\x01"
            struct.pack_into("<H", blob, 18, 62)
            path.write_bytes(blob)
        else:
            path.write_text("#!/bin/sh\nexit 0\n")
        path.chmod(0o755)
        return path

    def entry(self, path, **changes):
        entry = {"name": "httpx", "execution": "host", "platform": "darwin/arm64", "path": path.relative_to(self.data).as_posix(), "sha256": register.file_sha256(path)}
        return {**entry, **changes}

    def manifest(self, entries):
        path = Path(self.temporary.name) / "manifest.json"
        path.write_text(json.dumps({"tools": entries}))
        return path

    def test_mac_variant_supplies_absolute_httpx_path_and_ignores_target(self):
        native = self.executable("tools/httpx")
        target = self.executable("tools/targets/linux-amd64/chisel", "linux/amd64")
        entry = self.entry(native, variants=[{"platform": "linux/amd64", "path": "tools/httpx-linux", "sha256": "0" * 64}, {"platform": "darwin/arm64", "path": "tools/httpx", "sha256": register.file_sha256(native)}])
        target_entry = self.entry(target, name="chisel", execution="target", platform="linux/amd64")
        rows, skipped = register.registration_rows([self.manifest([entry, target_entry])], self.data, ["worker"], "darwin/arm64")
        self.assertEqual(skipped, 1)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["path"], str(native))
        self.assertIn(str(native), rows[0]["description"])
        self.assertEqual(rows[0]["key"], "external_httpx")

    def test_host_metadata_does_not_make_linux_binary_usable_on_mac(self):
        path = self.executable("tools/httpx", "linux/amd64")
        with self.assertRaisesRegex(register.RegistrationError, "architecture mismatch"):
            register.verified_path(self.entry(path), self.data, "darwin/arm64")

    def test_modified_tool_and_symlink_escape_are_rejected(self):
        path = self.executable("tools/httpx")
        entry = self.entry(path)
        path.write_bytes(path.read_bytes() + b"changed")
        with self.assertRaisesRegex(register.RegistrationError, "checksum mismatch"):
            register.verified_path(entry, self.data, "darwin/arm64")
        outside = Path(self.temporary.name) / "external"
        outside.write_bytes(path.read_bytes())
        outside.chmod(0o755)
        path.unlink()
        path.symlink_to(outside)
        with self.assertRaisesRegex(register.RegistrationError, "outside data/tools"):
            register.verified_path(entry, self.data, "darwin/arm64")

    def test_unpinned_files_and_legacy_unclassified_entries_are_not_registered(self):
        path = self.executable("tools/httpx")
        with self.assertRaisesRegex(register.RegistrationError, "pinned sha256"):
            register.verified_path(self.entry(path, sha256=""), self.data, "darwin/arm64")
        rows, skipped = register.registration_rows([self.manifest([self.entry(path, execution="")])], self.data, ["worker"], "darwin/arm64")
        self.assertEqual(rows, [])
        self.assertEqual(skipped, 1)

    def test_shell_hint_payload_remains_json_data_in_sql(self):
        dangerous = "'; DROP TABLE tools; -- $(TOKEN) `TOKEN`"
        rows = [{"key": "external_httpx", "description": dangerous, "agents": ["worker"], "path": "/data/tools/httpx"}]
        sql = register.insert_sql(rows)
        self.assertNotIn(dangerous, sql)
        self.assertIn("ON CONFLICT (key) DO NOTHING", sql)
        encoded = sql.split("decode('", 1)[1].split("'", 1)[0]
        self.assertEqual(json.loads(bytes.fromhex(encoded)), rows)
        self.assertNotIn("DO UPDATE", sql)

    def test_normalized_key_collision_is_rejected(self):
        first = self.executable("tools/first")
        second = self.executable("tools/second")
        entries = [self.entry(first, name="some-tool"), self.entry(second, name="some_tool")]
        with self.assertRaisesRegex(register.RegistrationError, "conflicting registered paths"):
            register.registration_rows([self.manifest(entries)], self.data, ["worker"], "darwin/arm64")

    def test_pinned_nmap_homebrew_bottle_runtime_is_allowed_and_source_build_is_rejected(self):
        wrapper = self.executable("tools/nmap", "script")
        prefix = Path(self.temporary.name) / "homebrew"
        command = prefix / "Cellar" / "nmap" / "7.98" / "bin" / "nmap"
        command.parent.mkdir(parents=True)
        command.write_bytes(struct.pack("<II", 0xFEEDFACF, 0x0100000C) + b"\x00" * 24)
        command.chmod(0o755)
        receipt = command.parent.parent / "INSTALL_RECEIPT.json"
        receipt.write_text(json.dumps({"poured_from_bottle": True, "source": {"tap": "homebrew/core"}}))
        entry = self.entry(wrapper, name="nmap", version="7.98", runtime_origin="homebrew-bottle", resolved_executable=str(command), resolved_sha256=register.file_sha256(command), homebrew_receipt_path=str(receipt), homebrew_receipt_sha256=register.file_sha256(receipt))
        with mock.patch.dict(register.HOMEBREW_PREFIXES, {"darwin/arm64": prefix}):
            self.assertEqual(register.verified_path(entry, self.data, "darwin/arm64"), wrapper)
            receipt.write_text(json.dumps({"poured_from_bottle": False, "source": {"tap": "homebrew/core"}}))
            entry["homebrew_receipt_sha256"] = register.file_sha256(receipt)
            with self.assertRaisesRegex(register.RegistrationError, "not a poured"):
                register.verified_path(entry, self.data, "darwin/arm64")

    def test_homebrew_origin_flag_does_not_allow_arbitrary_external_command(self):
        wrapper = self.executable("tools/nmap", "script")
        command = Path(self.temporary.name) / "outside-nmap"
        command.write_bytes(struct.pack("<II", 0xFEEDFACF, 0x0100000C) + b"\x00" * 24)
        command.chmod(0o755)
        entry = self.entry(wrapper, name="nmap", runtime_origin="homebrew-bottle", resolved_executable=str(command), resolved_sha256=register.file_sha256(command))
        with self.assertRaisesRegex(register.RegistrationError, "Homebrew Cellar"):
            register.verified_path(entry, self.data, "darwin/arm64")

    def test_nuclei_declaration_uses_installed_templates_and_disables_update_checks(self):
        executable = self.executable("tools/nuclei")
        rows, _ = register.registration_rows([self.manifest([self.entry(executable, name="nuclei")])], self.data, ["worker"], "darwin/arm64")
        self.assertIn("-t " + str(self.data / "tools" / "nuclei-templates"), rows[0]["description"])
        self.assertIn("-duc", rows[0]["description"])


if __name__ == "__main__":
    unittest.main()
