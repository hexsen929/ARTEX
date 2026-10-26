"""Supply-chain and file-preservation checks; these do not install any tools."""
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import zipfile


SPEC = importlib.util.spec_from_file_location("prepare_python_tools", Path(__file__).with_name("prepare-python-tools.py"))
PREPARE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PREPARE)


class PythonToolPreparationTests(unittest.TestCase):
    def test_source_build_is_rejected_outside_actions_before_mutation(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "not-created"
            args = mock.Mock(wheelhouse=destination)
            with mock.patch.dict("os.environ", {}, clear=True):
                with self.assertRaisesRegex(RuntimeError, "GitHub Actions"):
                    PREPARE.build_wheelhouse(args, {})
            self.assertFalse(destination.exists())

    def test_source_archive_rejects_parent_traversal_before_extracting(self):
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as output:
            output.writestr("root/normal.py", "pass")
            output.writestr("root/../../outside", "overwrite")
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "unpacked"
            with mock.patch.object(PREPARE, "fetch", return_value=archive.getvalue()):
                with self.assertRaisesRegex(RuntimeError, "Unsafe source"):
                    PREPARE.source_zip({"repository": "fixture/repo", "commit": "fixture", "sha256": "fixture"}, destination)
            self.assertFalse(destination.exists())
            self.assertFalse((Path(temporary) / "outside").exists())

    def test_source_archive_rejects_symlink(self):
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as output:
            link = zipfile.ZipInfo("root/link")
            link.create_system = 3
            link.external_attr = 0o120777 << 16
            output.writestr(link, "../../outside")
        with tempfile.TemporaryDirectory() as temporary:
            with mock.patch.object(PREPARE, "fetch", return_value=archive.getvalue()):
                with self.assertRaisesRegex(RuntimeError, "symlink"):
                    PREPARE.source_zip({"repository": "fixture/repo", "commit": "fixture", "sha256": "fixture"}, Path(temporary) / "unpacked")

    def test_user_file_is_preserved_and_identical_rerun_is_noop(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary).resolve() / "nxc"
            path.write_bytes(b"user custom file")
            before = path.stat()
            PREPARE.atomic_bytes(path, b"user custom file", 0o755)
            self.assertEqual(path.stat().st_ino, before.st_ino)
            with self.assertRaisesRegex(RuntimeError, "custom file"):
                PREPARE.atomic_bytes(path, b"replacement", 0o755)
            self.assertEqual(path.read_bytes(), b"user custom file")
            self.assertEqual(list(Path(temporary).resolve().iterdir()), [path])

    def test_wheel_payload_mutation_is_detected_before_installation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            group = root / "impacket"
            group.mkdir()
            wheel = group / "fixture-1.0-py3-none-any.whl"
            wheel.write_bytes(b"valid wheel payload fixture")
            requirement = group / "requirements.lock"
            requirement.write_text("fixture==1.0\n")
            index = {"platform": "darwin/arm64", "python": "3.11", "sources_sha256": PREPARE.digest(PREPARE.SOURCE_PATH), "groups": {"impacket": {"requirements_sha256": PREPARE.digest(requirement), "wheels": [{"file": wheel.name, "sha256": PREPARE.digest(wheel)}]}}}
            (root / "wheelhouse-index.json").write_text(json.dumps(index))
            with mock.patch.object(PREPARE, "host_platform", return_value="darwin/arm64"):
                PREPARE.validate_wheelhouse(root, {}, ["impacket"])
                wheel.write_bytes(b"changed wheel payload fixture")
                with self.assertRaisesRegex(RuntimeError, "checksum mismatch"):
                    PREPARE.validate_wheelhouse(root, {}, ["impacket"])

    def test_wrappers_scope_runtime_files_without_overriding_home(self):
        wrapper = PREPARE.make_wrapper(Path("/tools/python"), Path("/tools/nxc.py"), {"NXC_PATH": "/tools/runtime/nxc"}).decode()
        self.assertIn('if [ -z "${NXC_PATH:-}" ]', wrapper)
        self.assertIn("/tools/runtime/nxc", wrapper)
        self.assertNotIn("HOME=", wrapper)
        with self.assertRaisesRegex(RuntimeError, "Unsupported"):
            PREPARE.make_wrapper(Path("/tools/python"), Path("/tools/entry.py"), {"HOME": "/different-home"})

    def test_offline_requirements_reject_git_dependency_in_wheel(self):
        with tempfile.TemporaryDirectory() as temporary:
            wheel = Path(temporary) / "fixture.whl"
            with zipfile.ZipFile(wheel, "w") as archive:
                archive.writestr("fixture-1.0.dist-info/METADATA", "Name: fixture\nVersion: 1.0\nRequires-Dist: dependency @ git+https://example.invalid/dependency\n")
            with self.assertRaisesRegex(RuntimeError, "direct reference"):
                PREPARE.wheel_info(wheel)

    def test_registration_manifest_keeps_target_class_and_underlying_entry_hash(self):
        record = {"name": "lazagne-macos", "execution": "target", "path": "tools/lazagne-macos", "sha256": "wrapper-hash", "resolved_executable": "tools/targets/darwin-arm64/lazagne/laZagne.py", "resolved_sha256": "source-hash", "python_executable": "/runtime/python"}
        entry = PREPARE.manifest_entry(record)
        self.assertEqual(entry["execution"], "target")
        self.assertEqual(entry["resolved_executable"], record["resolved_executable"])
        self.assertEqual(entry["resolved_sha256"], "source-hash")
        self.assertNotIn("python_executable", entry)


if __name__ == "__main__":
    unittest.main()
