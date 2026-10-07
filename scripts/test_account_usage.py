import datetime
import importlib.util
from pathlib import Path
import unittest
import tempfile

spec=importlib.util.spec_from_file_location('reconcile',Path(__file__).with_name('reconcile-site.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class AccountUsageTests(unittest.TestCase):
    def setUp(self):
        self.now=datetime.datetime(2026,10,7,12,tzinfo=datetime.timezone.utc)
        self.scope={'github_url':'https://github.com/example','app_installation_id':11,'runner_group_id':4}
        self.repo={'id':42,'full_name':'example/project','owner':{'id':22}}
        self.visibility='selected';self.accessible=[self.repo];self.selected=[self.repo]
        self.record={'id':'one','github_url':self.scope['github_url'],'reserved_at':'2026-10-06T23:59:00Z','completed_at':'2026-10-07T00:01:00Z'}
    def api(self,path,*args,**kw):
        if path=='/app/installations/11':return {'id':11,'account':{'id':22,'type':'Organization','login':'example'},'permissions':{'organization_self_hosted_runners':'write'}}
        if path.endswith('/access_tokens'):return {'token':'fixture-not-secret'}
        if path.startswith('/installation/repositories'):return {'repositories':self.accessible}
        if path.endswith('/repositories?per_page=100'):return {'repositories':self.selected,'total_count':len(self.selected)}
        if path.endswith('/runner-groups/4'):return {'id':4,'visibility':self.visibility}
        raise AssertionError(path)
    def collect(self):return m.collect_account_usage({'scopes':{'primary':self.scope}},[self.record],'fixture',self.api,self.now)
    def test_primary_without_enrollment_and_midnight_split(self):
        values=self.collect();self.assertEqual(len(values),1)
        self.assertEqual(values[0]['repository_ids'],[42]);self.assertEqual(values[0]['account_id'],22)
        self.assertEqual(values[0]['observed_at'],self.now.isoformat())
        self.assertEqual(values[0]['usage'][-2],{'date':'2026-10-06','vm_seconds':60.0,'vms':0})
        self.assertEqual(values[0]['usage'][-1],{'date':'2026-10-07','vm_seconds':60.0,'vms':1})
    def test_acl_failure_omits_no_intersection(self):
        self.selected.append({'id':43,'owner':{'id':22}})
        self.assertEqual(self.collect(),[])
        self.selected=[self.repo];self.visibility='all'
        self.assertEqual(self.collect(),[])
    def test_wrong_account_and_suspended(self):
        self.repo['owner']['id']=99;self.assertEqual(self.collect(),[])
        self.repo['owner']['id']=22
        old=self.api
        def api(path,*a,**kw):
            data=old(path,*a,**kw)
            if path=='/app/installations/11':data['suspended_at']='2026-10-07'
            return data
        self.assertEqual(m.collect_account_usage({'scopes':{'primary':self.scope}},[],'fixture',api,self.now),[])
    def test_duplicate_ledger_or_scope_rejected(self):
        with self.assertRaises(ValueError):m.usage_days([self.record,self.record],self.scope['github_url'],self.now)
        self.assertEqual(m.collect_account_usage({'scopes':{'primary':self.scope,'other':self.scope}},[self.record],'fixture',self.api,self.now),[])
    def test_status_file_bound(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertIsNone(m.read_runner_status(directory))
            path=Path(directory)/'status.json';path.write_text('{"queues": []}')
            self.assertEqual(m.read_runner_status(directory),{'queues':[]})
            path.write_bytes(b' ' *((2<<20)+1))
            with self.assertRaises(ValueError):m.read_runner_status(directory)
    def test_runner_activity_fresh_empty_and_allocations(self):
        scope={**self.scope,'profiles':{'chickadee':{}}}
        status={'updated_at':self.now.isoformat(),'queues':[{'github_url':scope['github_url'],'label':'chickadee','allocated_vms':0,'credentialed_vms':0}]}
        self.assertEqual(m.runner_activity(status,scope,self.now),{'live_at':self.now.isoformat(),'runners':[]})
        status['queues'][0].update(allocated_vms=2,credentialed_vms=1)
        self.assertEqual(m.runner_activity(status,scope,self.now)['runners'],[{'label':'chickadee','allocated':2,'credentialed':1}])
        self.assertNotIn('github_url',str(m.runner_activity(status,scope,self.now)))
    def test_runner_activity_stale_missing_and_invalid(self):
        scope={**self.scope,'profiles':{'chickadee':{}}}
        self.assertEqual(m.runner_activity(None,scope,self.now),{})
        status={'updated_at':(self.now-datetime.timedelta(seconds=121)).isoformat(),'queues':[]}
        self.assertEqual(m.runner_activity(status,scope,self.now),{})
        status['updated_at']=(self.now+datetime.timedelta(seconds=61)).isoformat()
        self.assertEqual(m.runner_activity(status,scope,self.now),{})
        status['updated_at']=self.now.isoformat()
        queue={'github_url':scope['github_url'],'label':'chickadee','allocated_vms':1,'credentialed_vms':2}
        status['queues']=[queue]
        with self.assertRaises(ValueError):m.runner_activity(status,scope,self.now)
        queue['credentialed_vms']=1;status['queues']=[queue,queue]
        with self.assertRaises(ValueError):m.runner_activity(status,scope,self.now)
        queue['label']='unauthorized';status['queues']=[queue]
        with self.assertRaises(ValueError):m.runner_activity(status,scope,self.now)
        values=m.collect_account_usage({'scopes':{'primary':scope}},[self.record],'fixture',self.api,self.now,status)
        self.assertEqual(len(values),1);self.assertNotIn('live_at',values[0])
    def test_personal_identity_and_repo_access(self):
        scope={'github_url':'https://github.com/person/project','app_installation_id':11}
        repo={'id':42,'full_name':'person/project','owner':{'id':22}}
        def api(path,*a,**kw):
            if path=='/app/installations/11':return {'id':11,'account':{'id':22,'type':'User','login':'person'},'permissions':{'administration':'write'}}
            if path.endswith('/access_tokens'):return {'token':'fixture'}
            if path.startswith('/installation/repositories'):return {'repositories':[repo]}
            if path=='/repos/person/project':return repo
            raise AssertionError(path)
        v=m.account_usage_scope(scope,[],'fixture',api,self.now)
        self.assertEqual(v['account_type'],'User');self.assertEqual(v['repository_ids'],[42])

if __name__=='__main__':unittest.main()
