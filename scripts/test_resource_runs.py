import datetime,importlib.util,pathlib,unittest
spec=importlib.util.spec_from_file_location('resource_reconciler',pathlib.Path(__file__).with_name('reconcile-site.py'));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class ResourceRuns(unittest.TestCase):
 def test_scope_retention_and_bound(self):
  now=datetime.datetime.now(datetime.timezone.utc);scope={'github_url':'https://github.com/example'}
  def record(n,url=scope['github_url'],at=now):return {'id':str(n),'github_url':url,'completed_at':at.isoformat(),'label':'chickadee','cpus':2,'memory_mib':4096,'resources':{'version':1}}
  records=[record(i) for i in range(15)]+[record('other','https://github.com/private-other'),record('old',at=now-datetime.timedelta(days=8))]
  rows=m.resource_runs(records,scope,now)
  self.assertEqual(len(rows),10);self.assertNotIn('other',[r['id'] for r in rows]);self.assertNotIn('old',[r['id'] for r in rows])
  with self.assertRaises(ValueError):m.resource_runs([record(1),record(1)],scope,now)
  self.assertEqual(m.resource_runs([{'github_url':scope['github_url']}],scope,now),[])
