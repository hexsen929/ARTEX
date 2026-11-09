import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parent.parent / 'skills/artex-intent-cleanup'


class IntentCleanupTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which('lsof'), 'lsof is required for open-file protection checks')
    def test_disposable_cleanup_boundaries(self):
        passed = []

        with tempfile.TemporaryDirectory(prefix='artex-cleanup-validation-') as tmp:
            base = Path(tmp).resolve()
            skill = base / 'skill'
            shutil.copytree(SOURCE, skill)
            script = skill / 'scripts/cleanup_scratch.py'

            def call(root, *args, ok=True, env=None):
                result = subprocess.run([sys.executable, str(script), str(root), *args],
                                        capture_output=True, text=True, timeout=30, env=env)
                assert (result.returncode == 0) == ok, (args, result.stdout, result.stderr)
                return json.loads(result.stdout or result.stderr)

            counter = [0]
            def fixture():
                counter[0] += 1
                root = base / 'data/tasks' / str(counter[0]) / ('i' + str(counter[0]))
                root.mkdir(parents=True)
                call(root, '--init')
                scratch = root / '.artex-disposable'
                (scratch / 'discard.txt').write_text('regenerable scratch')
                (root / 'evidence.txt').write_text('persistent evidence')
                return root, scratch

            def plan(root, *names):
                args = ['--plan']
                for name in names or ('discard.txt',):
                    args += ['--file', name]
                return call(root, *args)['plan_path']

            def apply(root, path, ok=True, env=None):
                return call(root, '--apply-plan', str(path), '--work-complete', '--writers-stopped', ok=ok, env=env)

            empty = base / 'data/tasks/999/i999'
            empty.mkdir(parents=True)
            assert not call(empty)['initialized']
            assert not list(empty.iterdir())
            passed.append('default preview performs no writes')

            root, scratch = fixture()
            apply(root, plan(root), ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('preview policy refuses actual deletion')
            (skill / 'policy.json').write_text(json.dumps({'version': 1, 'mode': 'delete-disposable'}))

            root, scratch = fixture()
            outsider = root / 'evidence.txt'
            (scratch / 'link.txt').symlink_to(outsider)
            os.link(outsider, scratch / 'hardlink.txt')
            (scratch / 'nested').mkdir()
            (scratch / 'nested/keep.txt').write_text('nested data')
            inspected = call(root)
            assert {e['name'] for e in inspected['files']} == {'discard.txt'}
            for name in ['link.txt', 'hardlink.txt', 'nested', '../evidence.txt', '.artex-owner.json']:
                call(root, '--plan', '--file', name, ok=False)
            p = plan(root)
            (scratch / 'created-after-plan.txt').write_text('new scratch')
            result = apply(root, p)
            assert result['deleted_bytes'] == len('regenerable scratch')
            assert not (scratch / 'discard.txt').exists()
            assert outsider.read_text() == 'persistent evidence'
            assert (scratch / 'nested/keep.txt').exists()
            assert (scratch / 'created-after-plan.txt').exists()
            assert (root / '.artex-cleanup-audit.jsonl').exists()
            passed.append('only exact planned regular files deleted; evidence, links, directories and new files preserved')

            root, scratch = fixture()
            (scratch / 'second.txt').write_text('before')
            p = plan(root, 'discard.txt', 'second.txt')
            (scratch / 'second.txt').write_text('after change')
            apply(root, p, ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('any changed candidate blocks the batch before deletion')

            root, scratch = fixture()
            p = plan(root)
            with (scratch / 'discard.txt').open('rb') as held:
                apply(root, p, ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('an open file blocks deletion')

            root, scratch = fixture()
            p = plan(root)
            environment = os.environ.copy()
            environment['PATH'] = str(base / 'no-executables')
            apply(root, p, ok=False, env=environment)
            assert (scratch / 'discard.txt').exists()
            passed.append('missing lsof blocks deletion')

            root, scratch = fixture()
            p = plan(root)
            other, other_scratch = fixture()
            apply(other, p, ok=False)
            copied = other / Path(p).name
            shutil.copyfile(p, copied)
            apply(other, copied, ok=False)
            assert (scratch / 'discard.txt').exists() and (other_scratch / 'discard.txt').exists()
            passed.append('cross-intent and copied foreign plans rejected')

            root, scratch = fixture()
            p = plan(root)
            scratch.rename(root / 'old-scratch')
            call(root, '--init')
            (scratch / 'discard.txt').write_text('different directory')
            apply(root, p, ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('replaced scratch directory invalidates old plan')

            root, scratch = fixture()
            p = plan(root)
            (scratch / 'discard.txt').unlink()
            (scratch / 'discard.txt').symlink_to(root / 'evidence.txt')
            apply(root, p, ok=False)
            assert (root / 'evidence.txt').exists()
            passed.append('file replaced by symlink after planning cannot delete target')

            root, scratch = fixture()
            (scratch / '.artex-owner.json').unlink()
            call(root, '--init', ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('existing unmarked directory is never adopted')

            root, scratch = fixture()
            p = plan(root)
            call(root, '--apply-plan', p, ok=False)
            call(root, '--apply-plan', p, '--work-complete', ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('both completion declarations are required')

            root, scratch = fixture()
            p = plan(root)
            os.mkfifo(root / '.artex-cleanup-audit.jsonl')
            apply(root, p, ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('unsafe audit destination fails without blocking or deleting')

            root, scratch = fixture()
            p = Path(plan(root))
            saved = p.with_name('saved-plan.json')
            p.rename(saved)
            p.symlink_to(saved)
            apply(root, p, ok=False)
            assert (scratch / 'discard.txt').exists()
            passed.append('symlinked plan rejected')

            root, scratch = fixture()
            linked = root.parent / 'i888'
            linked.symlink_to(root, target_is_directory=True)
            call(linked, ok=False)
            call(root.parent, ok=False)
            passed.append('symlinked root and whole-task root rejected')

        assert json.loads((SOURCE / 'policy.json').read_text())['mode'] == 'preview'
        self.assertEqual(len(passed), 14)


if __name__ == '__main__':
    unittest.main()
