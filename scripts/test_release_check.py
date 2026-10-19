from contextlib import nullcontext
import importlib.util
import json
import os
import zipfile
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('release_check', Path(__file__).with_name('release-check.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseCheckTest(unittest.TestCase):
    def test_json_success_does_not_hide_findings(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(release, 'EVIDENCE', Path(directory)), \
                patch.object(release.subprocess, 'run', side_effect=[
                    subprocess.CompletedProcess([], 0),
                    subprocess.CompletedProcess([], 3),
                ]) as run:
            self.assertEqual(release.scan_one('scanner', 'source', ['./...']), 3)
            self.assertEqual(run.call_args.args[0], ['scanner', '-mode=convert'])

    def test_scanner_error_does_not_convert_incomplete_report(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(release, 'EVIDENCE', Path(directory)), \
                patch.object(release.subprocess, 'run', return_value=
                             subprocess.CompletedProcess([], 1)) as run:
            self.assertEqual(release.scan_one('scanner', 'source', ['./...']), 1)
            self.assertEqual(run.call_count, 1)

    def test_failure_still_scans_both_binaries(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(release, 'EVIDENCE', Path(directory)), \
                patch.object(release, 'output', return_value='scanner version'), \
                patch.object(release, 'database_snapshot', return_value=nullcontext('file:///test')), \
                patch.object(release, 'scan_one', side_effect=[3, 0, 0]) as scan, \
                patch('builtins.print'):
            self.assertEqual(release.scan(), 1)
            self.assertEqual(scan.call_count, 3)

    def test_stale_binary_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(release, 'EVIDENCE', Path(directory)), \
                patch.object(release, 'output', side_effect=[
                    'new-revision', '', 'go1.26.9 vcs.revision=old-revision']):
            with self.assertRaisesRegex(ValueError, 'vcs.revision=new-revision'):
                release.metadata()

    def test_snapshot_replay_retains_hash_and_database_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / 'saved.zip'
            with zipfile.ZipFile(archive, 'w') as bundle:
                bundle.writestr('index/db.json', '{"modified":"2026-10-08T22:31:09Z"}')
                bundle.writestr('index/modules.json', '[]')
            with patch.object(release, 'EVIDENCE', root), \
                    patch.dict(os.environ, {'VULNDB_ARCHIVE': str(archive)}):
                with release.database_snapshot() as url:
                    self.assertTrue(url.startswith('file:///'))
                record = json.loads((root / 'database.json').read_text())
                self.assertEqual(record['sha256'], release.digest(archive))
                self.assertTrue(record['replay'])
                self.assertEqual(record['database']['modified'], '2026-10-08T22:31:09Z')

    def test_snapshot_rejects_path_traversal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / 'saved.zip'
            with zipfile.ZipFile(archive, 'w') as bundle:
                bundle.writestr('../escaped.json', '{}')
            with patch.object(release, 'EVIDENCE', root), \
                    patch.dict(os.environ, {'VULNDB_ARCHIVE': str(archive)}):
                with self.assertRaisesRegex(ValueError, 'unexpected'):
                    with release.database_snapshot():
                        self.fail('unsafe snapshot accepted')
