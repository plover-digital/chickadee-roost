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
        self.observed_statuses=statuses

        def api(path, method="GET", token=None, body=None):
            if path == "/app/installations/2":
                return copy.deepcopy(self.installation)
            if path == "/app/installations/2/access_tokens":
                return {"token": "fixture-token"}
            if path == "/installation/repositories?per_page=100&page=1":
                return {"repositories": [] if getattr(self,'missing_repository',False) else [self.repository]}
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
            if getattr(self,'reject_install',False):raise m.LiveReloadRejected('fixture rejected reload')
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

    def legacy_primary(self):
        self.entry.pop('organization_access')
        self.entry['repository']['permissions']={'admin':True}

    def test_legacy_primary_owner_stop_preserves_scope_and_acknowledges_after_reload(self):
        self.legacy_primary()
        self.policy.pop('profile_catalog')
        for desired in ('paused','disconnected'):
            with self.subTest(desired=desired):
                self.config['scopes']['primary'].pop('disabled',None)
                self.entry['desired_state']=desired
                installs,statuses,helpers,events=self.run_reconcile()
                self.assertEqual(len(installs),1)
                self.assert_preserved_disabled(installs[0][0])
                self.assertEqual(installs[0][1],set())
                self.assertEqual(statuses[-1]['status'],desired)
                self.assertEqual(statuses[-1]['enabled_queues'],[])
                self.assertEqual(helpers,[])
                self.assertLess(events.index('install'),events.index('status'))
                installs,statuses,_,_=self.run_reconcile()
                self.assertEqual(installs,[])
                self.assertEqual(statuses[-1]['status'],desired)

    def test_legacy_primary_disabled_active_never_reports_capacity_or_resumes(self):
        self.legacy_primary()
        self.config['scopes']['primary']['disabled']=True
        installs,statuses,helpers,_=self.run_reconcile()
        self.assertEqual(installs,[])
        self.assertEqual(helpers,[])
        self.assertEqual(statuses[-1]['status'],'pending')
        self.assertEqual(statuses[-1]['enabled_queues'],[])

    def test_legacy_primary_explicit_resume_preserves_queues_limits_and_acl(self):
        self.legacy_primary()
        self.entry['status']='disconnected'
        self.entry['repository'].pop('permissions')
        self.config['scopes']['primary']['disabled']=True
        self.policy.pop('profile_catalog')
        extra=copy.deepcopy(self.config['scopes']['primary']['profiles']['chickadee'])
        self.config['scopes']['primary']['profiles']['chickadee-small-rocky-102']=extra
        original=copy.deepcopy(self.config)
        installs,statuses,helpers,events=self.run_reconcile()
        original['scopes']['primary'].pop('disabled')
        self.assertEqual(installs[0][0],original)
        self.assertEqual(installs[0][1],set())
        self.assertEqual(helpers,[])
        self.assertEqual(statuses[-1]['status'],'active')
        self.assertEqual(statuses[-1]['enabled_queues'],['chickadee'])
        self.assertNotIn('applied_organization_access',statuses[-1])
        self.assertLess(events.index('install'),len(events)-1)

    def test_legacy_primary_resume_requires_current_owner(self):
        self.legacy_primary()
        self.entry['status']='paused'
        self.config['scopes']['primary']['disabled']=True
        with patch.object(m.admit,'current_organization_owner',return_value=False):
            installs,statuses,helpers,_=self.run_reconcile()
        self.assertEqual(installs,[])
        self.assertEqual(helpers,[])
        self.assertTrue(self.config['scopes']['primary']['disabled'])
        self.assertEqual(statuses[-1]['status'],'permission-required')

    def test_legacy_primary_failed_resume_remains_retryable_and_preserves_acl_metadata(self):
        self.legacy_primary()
        self.entry['status']='disconnected'
        self.entry['enabled_workflow_access']='repository'
        self.entry['enabled_workflow_path']='.github/workflows/existing.yml'
        self.config['scopes']['primary']['disabled']=True
        self.reject_install=True
        with self.assertRaises(m.LiveReloadRejected):self.run_reconcile()
        self.assertEqual(self.observed_statuses,[])
        self.assertTrue(json.loads(self.config_path.read_text())['scopes']['primary']['disabled'])
        self.reject_install=False
        installs,statuses,_,_=self.run_reconcile()
        self.assertEqual(len(installs),1)
        self.assertEqual(statuses[-1]['status'],'active')
        self.assertNotIn('enabled_workflow_access',statuses[-1])
        self.assertNotIn('enabled_workflow_path',statuses[-1])

    def test_legacy_primary_repository_admin_cannot_stop_organization(self):
        self.legacy_primary()
        self.entry['desired_state']='disconnected'
        with patch.object(m.admit,'current_organization_owner',return_value=False):
            installs,statuses,helpers,_=self.run_reconcile()
        self.assertEqual(installs,[])
        self.assertEqual(helpers,[])
        self.assertEqual(statuses[-1]['status'],'permission-required')

    def test_legacy_primary_stop_requires_members_permission(self):
        self.legacy_primary()
        self.entry['desired_state']='disconnected'
        self.installation['permissions'].pop('members')
        installs,statuses,_,_=self.run_reconcile()
        self.assertEqual(installs,[])
        self.assertEqual(statuses[-1]['status'],'permission-required')

    def test_legacy_primary_missing_anchor_still_requires_owner(self):
        self.legacy_primary()
        self.entry['desired_state']='disconnected'
        self.missing_repository=True
        with patch.object(m.admit,'current_organization_owner',return_value=False) as owner:
            installs,statuses,_,_=self.run_reconcile()
        owner.assert_called_once()
        self.assertEqual(installs,[])
        self.assertEqual(statuses[-1]['status'],'permission-required')
        installs,statuses,_,_=self.run_reconcile()
        self.assert_preserved_disabled(installs[0][0])
        self.assertEqual(statuses[-1]['status'],'disconnected')

    def test_legacy_primary_suspended_stop_preserves_identity_even_without_members(self):
        self.legacy_primary()
        self.entry['desired_state']='disconnected'
        self.installation['suspended_at']='fixture'
        self.installation['permissions'].pop('members')
        installs,statuses,_,_=self.run_reconcile()
        self.assert_preserved_disabled(installs[0][0])
        self.assertEqual(installs[0][1],{'https://github.com/example'})
        self.assertEqual(statuses[-1]['status'],'disconnected')

    def test_legacy_primary_wrong_installation_cannot_stop_or_report_active(self):
        self.legacy_primary()
        self.entry['installation_id']=99
        for desired in ('active','disconnected'):
            self.entry['desired_state']=desired
            installs,statuses,helpers,_=self.run_reconcile()
            self.assertEqual(installs,[])
            self.assertEqual(helpers,[])
            self.assertEqual(statuses[-1]['status'],'permission-required')
            self.assertEqual(statuses[-1]['enabled_queues'],[])

    def test_cached_legacy_primary_identity_remains_trusted_for_stop(self):
        self.legacy_primary()
        self.entry['desired_state']='disconnected'
        self.assertEqual(m.configured_scope_name(self.config,self.entry),'primary')
        self.assertTrue(m.trusted_site_entry(self.entry,False,self.config))
        self.entry['installation_id']=99
        self.assertFalse(m.trusted_site_entry(self.entry,False,self.config))

    def test_legacy_primary_stop_revalidates_acting_owner_over_old_policy_seed(self):
        self.legacy_primary()
        self.entry['repository'].pop('permissions')
        old=copy.deepcopy(self.entry)
        old['user']={'id':99,'login':'former-manager'}
        self.policy['managed_requests']=[old]
        self.entry['desired_state']='disconnected'
        with patch.object(m.admit,'current_organization_owner',return_value=True) as owner:
            installs,statuses,_,_=self.run_reconcile()
        self.assertEqual(owner.call_args.args[0]['user'],self.entry['user'])
        self.assert_preserved_disabled(installs[0][0])
        self.assertEqual(statuses[-1]['status'],'disconnected')
        with self.assertRaises(ValueError):m.validate_scope_merge(old,self.entry,authenticated=False)
        changed=copy.deepcopy(self.entry)
        changed['repository']['id']=999
        with self.assertRaises(ValueError):m.validate_scope_merge(old,changed,authenticated=True)

    def test_legacy_owner_handoff_survives_resume_and_subsequent_reconciliations(self):
        config,entry,policy=copy.deepcopy((self.config,self.entry,self.policy))
        for snapshot in (True,False):
            with self.subTest(repository_permission_snapshot=snapshot):
                self.config,self.entry,self.policy=copy.deepcopy((config,entry,policy))
                self.legacy_primary()
                if not snapshot:self.entry['repository'].pop('permissions')
                old=copy.deepcopy(self.entry)
                old['user']={'id':99,'login':'former-manager'}
                self.policy['managed_requests']=[old]
                self.entry['desired_state']='disconnected'
                _,statuses,_,_=self.run_reconcile()
                self.assertEqual(statuses[-1]['status'],'disconnected')
                self.entry.update(status='disconnected',desired_state='active',enabled_queues=[])
                _,statuses,helpers,_=self.run_reconcile()
                self.assertEqual(statuses[-1]['status'],'active')
                self.assertEqual(helpers,[])
                # The durable operator seed intentionally still names the old
                # manager. Neither pending initialization nor later active
                # observations may undo the verified owner's handoff.
                for status in ('pending','active','active'):
                    self.entry.update(status=status,enabled_queues=[] if status=='pending' else ['chickadee'])
                    installs,statuses,helpers,_=self.run_reconcile()
                    self.assertEqual(installs,[])
                    self.assertEqual(helpers,[])
                    self.assertEqual(statuses[-1]['status'],'active')
                    self.assertNotIn('disabled',self.config['scopes']['primary'])
                self.assertEqual(self.policy['managed_requests'][0]['user'],old['user'])

    def test_existing_legacy_handoff_trust_cannot_provision_or_accept_imports(self):
        self.legacy_primary()
        self.entry['repository'].pop('permissions')
        self.assertTrue(m.trusted_site_entry(self.entry,True,self.config))
        self.assertFalse(m.trusted_site_entry(self.entry,True,{}))
        nonprimary=copy.deepcopy(self.config)
        nonprimary['scopes'][m.scope_name(self.entry)]=nonprimary['scopes'].pop('primary')
        self.assertFalse(m.trusted_site_entry(self.entry,True,nonprimary))
        for field,value in (('authenticated',False),('installation_id',99),('installation_id',True)):
            changed=copy.deepcopy(self.entry);changed[field]=value
            self.assertFalse(m.trusted_site_entry(changed,True,self.config))

    def test_legacy_primary_flat_active_config_remains_read_only(self):
        self.legacy_primary()
        primary=self.config.pop('scopes')['primary']
        self.config.update(primary)
        installs,statuses,helpers,_=self.run_reconcile()
        self.assertEqual(installs,[])
        self.assertEqual(helpers,[])
        self.assertEqual(statuses[-1]['status'],'active')

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
