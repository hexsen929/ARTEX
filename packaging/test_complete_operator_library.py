import io
import importlib.util
from pathlib import Path
import sqlite3
import sys
import tarfile
import tempfile
import unittest
sys.path.insert(0, str(Path(__file__).parent))
import complete_operator_library as complete

class CompleteLibraryTests(unittest.TestCase):
    def test_independent_skills_variants_and_portable_resources(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            db = sqlite3.connect(root / "fixture.db")
            db.execute("CREATE TABLE poc(code TEXT)"); db.execute("INSERT INTO poc VALUES('fixture')")
            db.commit(); db.close()
            entries = {
                "redteam/knowledge.db": (root / "fixture.db").read_bytes(),
                "skills/sample.md": b"---\nname: sample\ndescription: fixture\nrole: recon\n---\nroot body\n",
                "redteam/skills/sample.md": b"---\nname: sample\ndescription: variant\n---\nother body\n",
                "skills/core/SKILL.md": b"---\nname: core\ndescription: core\n---\ncore body\n",
                "redteam/toolkit/wordlists/sample.txt": b"fixture\n",
                "redteam/toolkit/httpx/httpx": b"\x7fELFfixture",
                "redteam/toolkit/vps/id_rsa": b"private fixture not activated",
                "redteam/toolkit/Behinder_v4.1/data.db": (root / "fixture.db").read_bytes(),
            }
            with tarfile.open(root / "fixture.tar.gz", "w:gz") as tar:
                for name, raw in entries.items():
                    member = tarfile.TarInfo(complete.basic.PREFIX + name); member.size = len(raw)
                    tar.addfile(member, io.BytesIO(raw))
            result = complete.complete_library(root / "fixture.tar.gz", root / "out")
            self.assertEqual(len(result["independent_skills"]), 3)
            self.assertIn(b"name: sample-redteam", (root / "out/skill-imports/sample-redteam/SKILL.md").read_bytes())
            self.assertEqual((root / "out/toolkit/wordlists/sample.txt").read_bytes(), b"fixture\n")
            self.assertEqual((root / "out/toolkit-legacy/httpx/httpx").read_bytes(), b"\x7fELFfixture")
            self.assertTrue((root / "out/toolkit/httpx/httpx").read_text().startswith("#!/bin/sh"))
            self.assertFalse((root / "out/toolkit-legacy/vps/id_rsa").exists())
            self.assertEqual(sum(x["status"] == "excluded_inactive_state" for x in result["toolkit"]), 2)
    def test_metadata_name_and_malformed_header(self):
        raw = b"---\n\n## name: named\ndescription: fixture\n# body\n"
        self.assertEqual(complete.metadata(raw)["name"], "named")
        result = complete.canonical_skill(raw, "named")
        self.assertTrue(result.startswith(b"---\nname: named\n"))
        self.assertIn(b"# body", result)
    def test_malformed_source_horizontal_rule_is_not_frontmatter_end(self):
        raw = b"---\n\n## name: named\ndescription: fixture\n\n# Guide\nfirst part\n\n---\n\n## Second\nlast part\n"
        result = complete.canonical_skill(raw, "named")
        self.assertTrue(result.startswith(b"---\nname: named\n"))
        self.assertIn(b"# Guide\nfirst part", result)
        self.assertIn(b"## Second\nlast part", result)
        self.assertNotIn(b"## name:", result)
    def test_clean_tool_database_preserves_protocols_not_connections(self):
        with sqlite3.connect(":memory:") as db:
            db.executescript("CREATE TABLE shells(id INTEGER,url TEXT,password TEXT); INSERT INTO shells VALUES(1,'fixture.invalid','private'); CREATE TABLE TransProtocol(id INTEGER,name TEXT,encode TEXT); INSERT INTO TransProtocol VALUES(1,'fixture','fixture-code');")
            raw, counts = complete.clean_behinder_database(db.serialize())
        with sqlite3.connect(":memory:") as db:
            db.deserialize(raw)
            self.assertEqual(db.execute("SELECT count(*) FROM shells").fetchone()[0], 0)
            self.assertEqual(db.execute("SELECT name FROM TransProtocol").fetchone()[0], "fixture")
        self.assertEqual(counts, {"shells":0, "TransProtocol":1})
