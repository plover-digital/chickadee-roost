import copy
import importlib.util
import pathlib
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('admit',pathlib.Path(__file__).with_name('admit-installation.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class OrganizationAccess(unittest.TestCase):
    def setUp(self):
        self.install={'id':2,'repository_selection':'all','account':{'id':3,'login':'example','type':'Organization'}}
        self.repos=[{'id':4,'full_name':'example/one','owner':{'id':3}},{'id':5,'full_name':'example/two','owner':{'id':3}}]
        self.request={'organization_access':{'repository_selection':'all'},'account':self.install['account'],'user':{'id':6,'login':'owner'}}
    def test_all_and_multiple_selected(self):
        self.assertEqual(m.organization_access(self.request,self.install,self.repos),{'repository_selection':'all','repository_ids':[],'workflows':[]})
        self.request['organization_access']={'repository_selection':'selected','repository_ids':[5,4],'workflows':['example/two/.github/workflows/ci.yml@refs/heads/main']}
        self.assertEqual(m.organization_access(self.request,self.install,self.repos)['repository_ids'],[4,5])
    def test_bad_and_outside_boundaries(self):
        for policy in [{'repository_selection':'all','repository_ids':[4]}, {'repository_selection':'selected','repository_ids':[]}, {'repository_selection':'selected','repository_ids':[True]}, {'repository_selection':'selected','repository_ids':[4,4]}, {'repository_selection':'selected','repository_ids':[99]}, {'repository_selection':'all','workflows':['other/repo/.github/workflows/ci.yml@refs/heads/main']}, {'repository_selection':'selected','repository_ids':[4],'workflows':['example/two/.github/workflows/ci.yml@refs/heads/main']}]:
            self.request['organization_access']=policy
            with self.assertRaises(ValueError):m.organization_access(self.request,self.install,self.repos)
        self.request['organization_access']={'repository_selection':'all'}
        self.install['repository_selection']='selected'
        with self.assertRaises(ValueError):m.organization_access(self.request,self.install,self.repos)
    def test_owner_role_identity_and_state(self):
        valid={'state':'active','role':'admin','user':{'id':6},'organization':{'id':3}}
        with patch.object(m.setup,'api',return_value=valid):self.assertTrue(m.current_organization_owner(self.request,'fixture'))
        for update in [{'role':'member'},{'state':'pending'},{'user':{'id':7}},{'organization':{'id':True}}]:
            with patch.object(m.setup,'api',return_value={**valid,**update}):self.assertFalse(m.current_organization_owner(self.request,'fixture'))
    def test_matching_group_is_read_only(self):
        policy={'repository_selection':'selected','repository_ids':[4,5],'workflows':[]}
        group={'id':7,**m.organization_group_plan('owned',policy)}
        def api(path,method='GET',token=None,body=None):
            self.assertEqual(method,'GET')
            return {'total_count':2,'repositories':self.repos} if '/repositories?' in path else group
        with patch.object(m.setup,'api',side_effect=api):
            self.assertEqual(m.apply_organization_group('/orgs/example/actions/runner-groups','fixture','owned',policy,{'runner_group_id':7}),group)
    def test_restricting_existing_all_group_empties_acl_before_unrestricting_workflows(self):
        policy={'repository_selection':'selected','repository_ids':[4,5],'workflows':[]}
        group={'id':7,'visibility':'all','allows_public_repositories':True,'restricted_to_workflows':True,'selected_workflows':['example/one/.github/workflows/ci.yml@refs/heads/main']}
        selected=[];writes=[]
        def api(path,method='GET',token=None,body=None):
            if method!='GET':writes.append((method,copy.deepcopy(body)))
            if method=='PATCH':group.update(body)
            if method=='PUT':selected[:]=[r for r in self.repos if r['id'] in body['selected_repository_ids']]
            return {'total_count':len(selected),'repositories':selected} if '/repositories?' in path else copy.deepcopy(group)
        with patch.object(m.setup,'api',side_effect=api):m.apply_organization_group('/orgs/example/actions/runner-groups','fixture','owned',policy,{'runner_group_id':7})
        self.assertEqual(writes[1],('PUT',{'selected_repository_ids':[]}))
        self.assertFalse(writes[2][1]['restricted_to_workflows'])
        self.assertEqual(writes[-1],('PUT',{'selected_repository_ids':[4,5]}))
    def test_primary_profiles_can_be_removed_and_readded_from_independent_catalog(self):
        templates={'chickadee':{'max_vms':2,'warm_pool':1},'other':{'max_vms':1,'warm_pool':0}}
        config={'scopes':{'primary':{'profiles':{'chickadee':templates['chickadee']}}},'limits':{'max_vms':3}}
        result=m.requested_profiles(config,{'queues':['other'],'max_vms':1},templates)
        self.assertEqual(set(result),{'chickadee','other'});self.assertEqual(templates['chickadee']['warm_pool'],1)

    def test_primary_update_preserves_identity_limits_and_one_scope(self):
        profile={'image':'ubuntu','resources':'small','max_vms':2,'warm_pool':1}
        config={'limits':{'max_vms':3},'scopes':{'primary':{'github_url':'https://github.com/example','app_installation_id':2,'runner_group_id':7,'max_vms':3,'profiles':{'chickadee':profile}}}}
        request={**self.request,'repository':self.repos[0],'queues':['chickadee','other']}
        result=m.proposed(config,request,self.install,self.repos[0],7,{'chickadee':profile,'other':profile})
        self.assertEqual(set(result['scopes']),{'primary'})
        self.assertEqual(result['scopes']['primary']['max_vms'],3)
        self.assertEqual(result['scopes']['primary']['profiles']['chickadee'],profile)
        self.assertEqual(set(config['scopes']['primary']['profiles']),{'chickadee'})
