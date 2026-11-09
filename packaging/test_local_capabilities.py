import json
import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parent.parent


class LocalCapabilityTests(unittest.TestCase):
    def test_profile_references_shipped_skills_and_contains_no_machine_paths(self):
        path = ROOT / 'packaging/local-capabilities/profile.json'
        text = path.read_text()
        profile = json.loads(text)
        self.assertNotIn('/Users/', text)
        self.assertEqual(len({row['key'] for row in profile['tools']}), len(profile['tools']))
        for row in profile['skill_bindings']:
            self.assertTrue((ROOT / 'skills' / row['skill_name'] / 'SKILL.md').is_file())
        for row in profile['tools']:
            if row['key'].startswith('shodan_'):
                self.assertIn('key=${SHODAN_API_KEY}', row['exec']['url'])
                self.assertNotIn('api.shodan.io', row['description'])

    def test_shodan_without_key_fails_before_calling_curl(self):
        environment = dict(os.environ)
        environment.pop('SHODAN_API_KEY', None)
        # The explicit Bash executable works even when PATH has no curl.
        environment['PATH'] = '/nonexistent-artifact-test-path'
        result = subprocess.run(['/bin/bash', str(ROOT / 'skills/shodan/scripts/shodan'), 'info'],
                                env=environment, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertIn('SHODAN_API_KEY', result.stderr)
        self.assertEqual(result.stdout, '')

    def test_cleanup_remains_preview_by_default(self):
        path = ROOT / 'skills/artex-intent-cleanup/policy.json'
        self.assertEqual(json.loads(path.read_text()), {'version': 1, 'mode': 'preview'})

    def test_mcp_templates_keep_local_credentials_as_placeholders(self):
        folder = ROOT / 'packaging/local-capabilities'
        profile = json.loads((folder / 'mcp-profile.json').read_text())
        names = {server['name'] for server in profile['servers']}
        self.assertTrue(all(binding['server_name'] in names for binding in profile['bindings']))
        configs = json.loads((folder / 'browser-config-templates.json').read_text())['templates']
        cloak = next(server for server in profile['servers'] if server['name'] == 'browser-cloak')
        self.assertIn('--isolated', cloak['args'])
        self.assertIs(configs['browser-mcp-cloak.json']['browser']['isolated'], True)
        for config in configs.values():
            proxy = config['browser']['launchOptions']['proxy']
            self.assertEqual(proxy['password'], '${LOCAL_PROXY_PASSWORD}')
            self.assertEqual(proxy['server'], '${LOCAL_PROXY_SERVER}')
        for filename in ['mcp-profile.json', 'browser-config-templates.json']:
            self.assertNotIn('/Users/', (folder / filename).read_text())


if __name__ == '__main__':
    unittest.main()
