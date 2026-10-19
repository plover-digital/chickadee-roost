import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    'reviewed_recipe', Path(__file__).with_name('run-reviewed-recipe.py'))
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class ReviewedRecipeTest(unittest.TestCase):
    def test_dirty_helper_and_head_movement_cannot_change_attributed_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            scripts = root / 'scripts'
            scripts.mkdir()
            helper = b"RESULT = 'reviewed gate'\n"
            (scripts / 'release-check.py').write_bytes(helper)
            (scripts / 'run-reviewed-recipe.py').write_bytes(Path(launcher.__file__).read_bytes())
            recipe = b'''set -eu
python3 - <<'END'
import base64, json, os, pathlib
helper = base64.b64decode(os.environ['REVIEWED_HELPER_B64'], validate=True)
namespace = {}
exec(compile(helper, 'reviewed-helper', 'exec'), namespace)
root = pathlib.Path(os.environ['RELEASE_DIR'])
(root / 'result.json').write_text(json.dumps({
    'result': namespace['RESULT'],
    'provenance': json.loads(os.environ['RECIPE_PROVENANCE_JSON'])}))
END
'''
            (scripts / 'compiler-only-release.sh').write_bytes(recipe)
            subprocess.run(['git', 'init', '-q', str(root)], check=True)
            def git(*args):
                return subprocess.check_output(['git', '-C', str(root), *args], text=True).strip()
            git('config', 'user.email', 'fixture@example.invalid')
            git('config', 'user.name', 'Fixture')
            git('add', 'scripts')
            git('commit', '-qm', 'reviewed')
            revision = git('rev-parse', 'HEAD')
            # These modifications must never become the executable recipe/helper.
            (scripts / 'release-check.py').write_text("raise RuntimeError('dirty gate')\n")
            (scripts / 'compiler-only-release.sh').write_text('exit 91\n')
            original_run = subprocess.run
            def move_head_then_run(args, **kwargs):
                if args == ['bash', '-s']:
                    # Simulate a checkout update between input capture and execution.
                    original_run(['git', '-C', str(root), 'add', 'scripts'], check=True)
                    original_run(['git', '-C', str(root), 'commit', '-qm', 'concurrent change'], check=True)
                return original_run(args, **kwargs)
            cwd = Path.cwd()
            try:
                os.chdir(root)
                with patch.object(launcher.subprocess, 'run', side_effect=move_head_then_run):
                    self.assertEqual(launcher.run(revision), 0)
            finally:
                os.chdir(cwd)
            self.assertNotEqual(git('rev-parse', 'HEAD'), revision)
            output = next((root / 'build').iterdir())
            evidence = json.loads((output / 'result.json').read_text())
            self.assertEqual(evidence['result'], 'reviewed gate')
            self.assertEqual(evidence['provenance']['revision'], revision)
            for name, data in [('release-check.py', helper), ('compiler-only-release.sh', recipe)]:
                self.assertEqual((output / 'recipe' / name).read_bytes(), data)
                self.assertEqual(evidence['provenance']['files']['scripts/' + name]['sha256'],
                                 hashlib.sha256(data).hexdigest())

    def test_symbolic_or_abbreviated_revision_is_rejected(self):
        for revision in ['HEAD', 'main', '102fd51', 'a' * 40 + ':path']:
            with self.subTest(revision=revision), self.assertRaises(ValueError):
                launcher.reviewed_inputs(revision)
