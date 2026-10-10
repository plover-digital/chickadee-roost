import importlib.util
from pathlib import Path
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('reconcile',Path(__file__).with_name('reconcile-site.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class FleetValidator(unittest.TestCase):
    def test_fleet_catalog_never_uses_linux_vm_validator(self):
        with tempfile.TemporaryDirectory() as directory:
            fleet=Path(directory)/'fleet.json';fleet.write_text('{}')
            command=m.validation_command('/private/catalog.json',str(fleet))
            self.assertEqual(command,['/usr/local/bin/chickadee-roost','-catalog','/private/catalog.json','-fleet',str(fleet),'-check'])
    def test_explicit_missing_fleet_config_fails_closed(self):
        with self.assertRaises(ValueError):m.validation_command('/private/catalog.json','/missing/fleet.json')
    def test_standalone_keeps_its_existing_validator(self):
        self.assertEqual(m.validation_command('/private/catalog.json'),['/usr/local/bin/chickadee','-config','/private/catalog.json','-check'])
