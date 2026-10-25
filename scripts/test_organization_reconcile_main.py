"""Exercise organization stop/recovery decisions through the full reconciler."""
import builtins
import contextlib
import copy
import datetime
import importlib.util
import io
import json
import pathlib
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "organization_reconcile_main", pathlib.Path(__file__).with_name("reconcile-site.py")
)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class OrganizationReconcileMain(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="organization-reconcile-test-")
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.runtime = self.root / "runtime"
        self.runtime.mkdir()
        self.state = self.root / "managed"
        self.state.mkdir(mode=0o700)
        self.config_path = self.root / "config.json"
        self.policy_path = self.root / "policy.json"
        self.config = {
            "app_client_id": "fixture",
            "app_key_file": "unused-fixture-key",
            "state_dir": str(self.runtime),
            "limits": {"max_vms": 3},
            "scopes": {
                "primary": {
                    "github_url": "https://github.com/example",
                    "app_installation_id": 2,
                    "runner_group_id": 7,
                    "max_vms": 3,
                    "profiles": {
                        "chickadee": {
                            "image": "ubuntu-2404",
                            "resources": "small",
                            "max_vms": 2,
                            "warm_pool": 1,
                        }
                    },
                }
            },
        }
        self.entry = {
            "id": "fixture",
            "authenticated": True,
            "user": {"id": 6, "login": "owner"},
            "account": {"id": 3, "type": "Organization", "login": "example"},
            "installation_id": 2,
            "repository": {"id": 4, "full_name": "example/repo"},
            "organization_access": {"repository_selection": "all"},
            "queues": ["chickadee"],
            "enabled_queues": ["chickadee"],
            "desired_state": "active",
        }
        self.installation = {
            "id": 2,
            "repository_selection": "all",
            "account": self.entry["account"],
            "permissions": {
                "organization_self_hosted_runners": "write",
                "members": "read",
            },
        }
        self.policy = {
            "auto_approve_authenticated": True,
            "live_reload": True,
            "profile_catalog": copy.deepcopy(
                self.config["scopes"]["primary"]["profiles"]
            ),
        }
        self.repository = {
            **self.entry["repository"], "owner": {"id": 3}, "private": True
        }
        (self.runtime / "status.json").write_text(json.dumps({
            "updated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "queues": [{
                "github_url": "https://github.com/example",
                "label": "chickadee",
                "queue_initialized": True,
            }],
        }))

    def run_reconcile(self):
        self.last_original = copy.deepcopy(self.config)
        self.config_path.write_text(json.dumps(self.config))
        self.policy_path.write_text(json.dumps(self.policy))
        self.policy_path.chmod(0o600)
        installs, statuses, helper_inputs, events = [], [], [], []

        def api(path, method="GET", token=None, body=None):
            if path == "/app/installations/2":
                return copy.deepcopy(self.installation)
            if path == "/app/installations/2/access_tokens":
                return {"token": "fixture-token"}
            if path == "/installation/repositories?per_page=100&page=1":
                return {"repositories": [self.repository]}
            if path == "/orgs/example/memberships/owner":
                return {
                    "state": "active", "role": "admin",
                    "organization": self.entry["account"], "user": self.entry["user"],
                }
            raise AssertionError("Unexpected GitHub call: " + path)

        def run(command, **kwargs):
            if command[0] == "ssh":
                endpoint = command[-1].rsplit("http://localhost/", 1)[-1]
                if endpoint == "enrollments":
                    return subprocess.CompletedProcess(
                        command, 0, json.dumps([self.entry]).encode()
                    )
                if endpoint == "status":
                    statuses.append(json.loads(kwargs["input"]))
                    events.append("status")
                elif endpoint != "account-usage":
                    raise AssertionError("Unexpected admin call: " + endpoint)
                return subprocess.CompletedProcess(command, 0, b"")
            if command[0] == "python3" and command[1].endswith("admit-installation.py"):
                def argument(flag):
                    return command[command.index(flag) + 1]
                config = json.loads(pathlib.Path(argument("--config")).read_text())
                request = json.loads(pathlib.Path(argument("--request")).read_text())
                catalog = json.loads(pathlib.Path(argument("--profile-catalog")).read_text())
                helper_inputs.append(copy.deepcopy(config))
                # Use the real config proposal logic, preserving the known group.
                output = m.admit.proposed(
                    config, request, self.installation, self.repository, 7, catalog
                )
                pathlib.Path(argument("--output")).write_text(json.dumps(output))
                return subprocess.CompletedProcess(command, 0)
            raise AssertionError("Unexpected subprocess: " + repr(command))

        def install(candidate, path, quarantine, **kwargs):
            installs.append((copy.deepcopy(candidate), set(quarantine), kwargs))
            events.append("install")
            path.write_text(json.dumps(candidate))

        real_open, real_mkdir, real_stat = builtins.open, pathlib.Path.mkdir, pathlib.Path.stat
        lock_handle = io.StringIO()

        def open_fixture(path, *args, **kwargs):
            if str(path) == "/run/chickadee-managed/lock":
                return lock_handle
            return real_open(path, *args, **kwargs)

        def mkdir_fixture(path, *args, **kwargs):
            if str(path) != "/run/chickadee-managed":
                return real_mkdir(path, *args, **kwargs)

        def stat_fixture(path, *args, **kwargs):
            result = real_stat(path, *args, **kwargs)
            if path == self.state:
                return types.SimpleNamespace(st_uid=0, st_mode=result.st_mode)
            return result

        argv = [
            "reconcile-site.py", "--config", str(self.config_path),
            "--policy", str(self.policy_path), "--state-dir", str(self.state),
            "--site-host", "example.invalid", "--ssh-key", "unused-fixture-key",
            "--known-hosts", "unused-fixture-known-hosts",
        ]
        with contextlib.ExitStack() as stack:
            stack.callback(lock_handle.close)
            stack.enter_context(patch.object(sys, "argv", argv))
            stack.enter_context(patch.object(builtins, "open", side_effect=open_fixture))
            stack.enter_context(patch.object(pathlib.Path, "mkdir", new=mkdir_fixture))
            stack.enter_context(patch.object(pathlib.Path, "stat", new=stat_fixture))
            stack.enter_context(patch.object(m.os, "geteuid", return_value=0))
            stack.enter_context(patch.object(m.fcntl, "flock"))
            stack.enter_context(patch.object(m.admit.setup, "app_jwt", return_value="fixture-jwt"))
            stack.enter_context(patch.object(m.admit.setup, "api", side_effect=api))
            stack.enter_context(patch.object(m.subprocess, "run", side_effect=run))
            stack.enter_context(patch.object(m, "install_config", side_effect=install))
            stack.enter_context(patch.object(m, "collect_account_usage", return_value=[]))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            m.main()
        self.config = json.loads(self.config_path.read_text())
        return installs, statuses, helper_inputs, events

    def assert_preserved_disabled(self, candidate):
        expected = copy.deepcopy(self.last_original)
        expected["scopes"]["primary"]["disabled"] = True
        self.assertEqual(candidate, expected)
        self.assertEqual(set(candidate["scopes"]), {"primary"})

    def test_missing_members_disables_active_scope_and_honors_stop_intent(self):
        del self.installation["permissions"]["members"]
        for desired in ("active", "paused", "disconnected"):
            with self.subTest(desired=desired):
                self.config["scopes"]["primary"].pop("disabled", None)
                self.entry["desired_state"] = desired
                installs, statuses, helpers, events = self.run_reconcile()
                self.assertEqual(len(installs), 1)
                self.assert_preserved_disabled(installs[0][0])
                self.assertEqual(installs[0][1], set())
                self.assertTrue(installs[0][2]["live_reload"])
                self.assertEqual(statuses[-1]["status"],
                                 "permission-required" if desired == "active" else desired)
                self.assertEqual(statuses[-1]["enabled_queues"], [])
                self.assertEqual(helpers, [])
                self.assertLess(events.index("install"), events.index("status"))

    def test_missing_profile_catalog_cannot_block_stop_or_revocation(self):
        del self.policy["profile_catalog"]
        for desired, suspended in (("paused", False), ("disconnected", False), ("active", True)):
            with self.subTest(desired=desired, suspended=suspended):
                self.config["scopes"]["primary"].pop("disabled", None)
                self.entry["desired_state"] = desired
                self.installation["suspended_at"] = "fixture" if suspended else None
                installs, statuses, helpers, _ = self.run_reconcile()
                self.assertEqual(len(installs), 1)
                self.assert_preserved_disabled(installs[0][0])
                self.assertEqual(statuses[-1]["status"],
                                 "permission-required" if suspended else desired)
                self.assertEqual(helpers, [])

    def test_primary_suspension_preserves_identity_limits_templates_and_is_idempotent(self):
        original = copy.deepcopy(self.config)
        self.installation["suspended_at"] = "fixture"
        installs, statuses, helpers, _ = self.run_reconcile()
        expected = copy.deepcopy(original)
        expected["scopes"]["primary"]["disabled"] = True
        self.assertEqual(installs[0][0], expected)
        self.assertEqual(installs[0][1], {"https://github.com/example"})
        self.assertEqual(statuses[-1]["status"], "permission-required")
        self.assertEqual(helpers, [])
        installs, statuses, helpers, _ = self.run_reconcile()
        self.assertEqual(installs, [])  # No repeated drain/quarantine transaction.
        self.assertEqual(self.config, expected)
        self.assertEqual(statuses[-1]["enabled_queues"], [])

    def test_restored_permission_resumes_same_owned_primary(self):
        original = copy.deepcopy(self.config)
        self.config["scopes"]["primary"]["disabled"] = True
        installs, statuses, helpers, events = self.run_reconcile()
        self.assertEqual(len(helpers), 1)
        self.assertEqual(helpers[0]["scopes"]["primary"], {
            **original["scopes"]["primary"], "disabled": True
        })
        self.assertEqual(installs[0][0], original)
        self.assertEqual(installs[0][1], set())
        self.assertEqual(statuses[-1]["status"], "active")
        self.assertEqual(statuses[-1]["enabled_queues"], ["chickadee"])
        self.assertEqual(statuses[-1]["applied_organization_access"],
                         self.entry["organization_access"])
        self.assertLess(events.index("install"), len(events) - 1)

    def test_primary_queue_removal_status_never_reports_unrequested_old_labels(self):
        extra='chickadee-small-rocky-102'
        profile=copy.deepcopy(self.config['scopes']['primary']['profiles']['chickadee'])
        self.config['scopes']['primary']['profiles'][extra]=profile
        self.policy['profile_catalog'][extra]=profile
        installs,statuses,helper_inputs,events=self.run_reconcile()
        self.assertEqual(set(installs[0][0]['scopes']['primary']['profiles']),{'chickadee'})
        self.assertTrue(any(status['status']=='approved' for status in statuses))
        for status in statuses:self.assertLessEqual(set(status['enabled_queues']),set(self.entry['queues']))


if __name__ == "__main__":
    unittest.main()
