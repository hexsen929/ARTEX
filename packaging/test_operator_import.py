import importlib.util
import io
from pathlib import Path
import sqlite3
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("operator_import", Path(__file__).with_name("import_operator_resources.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class OperatorImportTests(unittest.TestCase):
    def test_resources_are_data_not_installers(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            db = sqlite3.connect(root / "fixture.db")
            db.execute("CREATE TABLE poc(code TEXT)")
            db.execute("INSERT INTO poc VALUES('fixture')")
            db.commit(); db.close()
            with tarfile.open(root / "fixture.tar.gz", "w:gz") as archive:
                for name, content in [("redteam/knowledge.db", (root / "fixture.db").read_bytes()),
                                      ("install-deps.sh", b"exit 99"), ("redteam/toolkit/tool", b"ELF"),
                                      ("redteam/pocs/fixture/poc.py", b"print('fixture')")]:
                    info = tarfile.TarInfo(module.PREFIX + name); info.size = len(content)
                    archive.addfile(info, io.BytesIO(content))
            manifest = module.import_resources(root / "fixture.tar.gz", root / "out")
            self.assertEqual(manifest["knowledge_entries"], 1)
            self.assertFalse((root / "out/install-deps.sh").exists())
            self.assertFalse((root / "out/toolkit/tool").exists())
            self.assertEqual((root / "out/pocs/fixture/poc.py").stat().st_mode & 0o111, 0)
    def test_selected_symlinks_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            with tarfile.open(root / "bad.tar.gz", "w:gz") as archive:
                info = tarfile.TarInfo(module.PREFIX + "redteam/pocs/escape")
                info.type = tarfile.SYMTYPE; info.linkname = "/outside"
                archive.addfile(info)
            with self.assertRaises(ValueError):
                module.import_resources(root / "bad.tar.gz", root / "out")
