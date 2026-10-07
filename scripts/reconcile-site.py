#!/usr/bin/env python3
"""Operator-owned beta reconciler. No public admin HTTP endpoint or database.

Run as root on the runner host. Site transport is pinned SSH + private Unix
socket. Policy admits trusted user IDs and explicitly approved queues only.
"""
import argparse, copy, fcntl, importlib.util, hashlib, json, os, pathlib, re, subprocess, tempfile, time

spec=importlib.util.spec_from_file_location('admit',pathlib.Path(__file__).with_name('admit-installation.py'))
admit=importlib.util.module_from_spec(spec);spec.loader.exec_module(admit)


def fleet_telemetry(runtime, now=None):
    """Forward only fresh bounded global counts; never tenant or worker identities."""
    import datetime
    now = now or datetime.datetime.now(datetime.timezone.utc)
    path = pathlib.Path(runtime) / 'status.json'
    if not path.exists():return None
    if path.stat().st_size > 2 << 20:raise ValueError('fleet status too large')
    status = json.loads(path.read_text())
    sample = status.get('fleet')
    if sample is None:return None  # Compatible with older controllers.
    fields = ('ready','booting','reserved','running','uncertain','workers_online','workers_total')
    if not isinstance(sample,dict) or set(sample) != {'at',*fields}:raise ValueError('invalid fleet telemetry')
    at = datetime.datetime.fromisoformat(sample['at'].replace('Z','+00:00'))
    if at.tzinfo is None:raise ValueError('fleet timestamp needs timezone')
    age = (now-at).total_seconds()
    if age > 120 or age < -60:return None
    for key in fields:
        limit = 64 if key.startswith('workers_') else 1024
        if type(sample[key]) is not int or not 0 <= sample[key] <= limit:raise ValueError('invalid fleet count')
    if sample['workers_online'] > sample['workers_total']:raise ValueError('invalid worker coverage')
    return sample


def usage_days(records, url, now):
    import datetime
    today=now.replace(hour=0,minute=0,second=0,microsecond=0)
    intervals=[];seen=set()
    if not isinstance(records,list) or len(records)>10000:raise ValueError('invalid usage ledger')
    for record in records:
        if record.get('github_url','').lower().rstrip('/')!=url.lower().rstrip('/'):continue
        rid=record.get('id')
        if not isinstance(rid,str) or not rid or rid in seen:raise ValueError('invalid duplicate usage record')
        seen.add(rid)
        start=datetime.datetime.fromisoformat(record['reserved_at'].replace('Z','+00:00'))
        end=datetime.datetime.fromisoformat(record['completed_at'].replace('Z','+00:00'))
        if start.tzinfo is None or end.tzinfo is None or end<start or (end-start).total_seconds()>25*3600 or end>now+datetime.timedelta(minutes=1):raise ValueError('invalid usage interval')
        intervals.append((start,end))
    days=[]
    for offset in range(-6,1):
        start=today+datetime.timedelta(days=offset);end=start+datetime.timedelta(days=1)
        days.append({'date':start.date().isoformat(),'vm_seconds':sum(max(0,(min(b,end)-max(a,start)).total_seconds()) for a,b in intervals),'vms':sum(start<=b<end for _,b in intervals)})
    return days


def account_usage_scope(scope, records, jwt, api, now):
    """Verify complete ACL; never intersect a wider group's usage into a subset."""
    iid=scope['app_installation_id']
    if type(iid) is not int or iid<=0:raise ValueError('invalid installation')
    match=re.fullmatch(r'https://github\.com/([A-Za-z0-9_.-]+)(?:/([A-Za-z0-9_.-]+))?/?',scope['github_url'])
    if not match:raise ValueError('invalid scope URL')
    owner,repo=match.groups();kind='User' if repo else 'Organization'
    installation=api('/app/installations/'+str(iid),token=jwt)
    account=installation['account'];aid=account['id']
    required='administration' if repo else 'organization_self_hosted_runners'
    if installation.get('id')!=iid or type(aid) is not int or aid<=0 or account.get('type')!=kind or account.get('login','').lower()!=owner.lower() or installation.get('suspended_at') or installation.get('permissions',{}).get(required)!='write':raise ValueError('installation identity or permission changed')
    token=api('/app/installations/'+str(iid)+'/access_tokens','POST',jwt,{'permissions':{required:'write','metadata':'read'}})['token']
    accessible={};page=1
    while True:
        listing=api('/installation/repositories?per_page=100&page='+str(page),token=token)
        repositories=listing['repositories']
        if not isinstance(repositories,list) or len(repositories)>100:raise ValueError('invalid repository list')
        for item in repositories:
            rid=item['id']
            if type(rid) is not int or rid<=0 or rid in accessible:raise ValueError('invalid repository identity')
            accessible[rid]=item
        # Bounded accessible installation inventory; never silently truncate.
        if len(accessible)>1000:raise ValueError('installation repository inventory exceeds limit')
        if len(repositories)<100:break
        page+=1
        if page>10:raise ValueError('installation repository inventory exceeds limit')
    if repo:
        selected=[api('/repos/'+owner+'/'+repo,token=token)]
        if selected[0].get('full_name','').lower()!=(owner+'/'+repo).lower():raise ValueError('repository identity changed')
    else:
        group_id=scope['runner_group_id']
        if type(group_id) is not int or group_id<=0:raise ValueError('invalid group identity')
        base='/orgs/'+owner+'/actions/runner-groups/'+str(group_id)
        group=api(base,token=token)
        if group.get('id')!=group_id or group.get('visibility')!='selected':raise ValueError('usage requires selected group visibility')
        listing=api(base+'/repositories?per_page=100',token=token)
        selected=listing['repositories']
        if type(listing.get('total_count')) is not int or listing['total_count']!=len(selected):raise ValueError('selected ACL incomplete')
    if not isinstance(selected,list) or not 1<=len(selected)<=100:raise ValueError('invalid selected ACL size')
    ids=[]
    for item in selected:
        rid=item['id']
        if type(rid) is not int or rid<=0 or rid in ids or rid not in accessible or item.get('owner',{}).get('id')!=aid or accessible[rid].get('owner',{}).get('id')!=aid:raise ValueError('selected ACL outside installation or account')
        ids.append(rid)
    return {'installation_id':iid,'account_id':aid,'account_type':kind,'repository_ids':sorted(ids),'observed_at':now.isoformat(),'usage':usage_days(records,scope['github_url'],now)}


def read_runner_status(runtime):
    path=pathlib.Path(runtime)/'status.json'
    if not path.exists():return None
    with path.open('rb') as file:raw=file.read((2<<20)+1)
    if len(raw)>2<<20:raise ValueError('runner status exceeds limit')
    return json.loads(raw)


def initialized_queues(status, url, requested, now):
    """Catalog acceptance does not prove listener initialization or job execution.

    Fresh older-controller entries without queue_initialized retain compatibility;
    missing/stale reports, absent queues and malformed flags fail closed.
    """
    import datetime
    if not isinstance(status,dict):return []
    try:
        at=datetime.datetime.fromisoformat(status['updated_at'].replace('Z','+00:00'))
        if at.tzinfo is None or not -60 <= (now-at).total_seconds() <= 120:return []
        queues=status['queues']
        if not isinstance(queues,list) or len(queues)>2048:return []
        expected=url.lower().rstrip('/')
        ready=set()
        for queue in queues:
            if not isinstance(queue,dict):return []
            if queue.get('github_url','').lower().rstrip('/') != expected:continue
            if queue.get('queue_initialized', True) is True:ready.add(queue.get('label'))
        return [label for label in requested if label in ready]
    except (KeyError,ValueError,TypeError,AttributeError):return []


def runner_activity(status, scope, now):
    import datetime
    if status is None:return {}
    at=datetime.datetime.fromisoformat(status['updated_at'].replace('Z','+00:00'))
    if at.tzinfo is None:raise ValueError('runner status timestamp requires timezone')
    age=(now-at).total_seconds()
    if age>120 or age < -60:return {}
    queues=status['queues']
    if not isinstance(queues,list) or len(queues)>2048:raise ValueError('invalid runner status queues')
    expected=scope['github_url'].lower().rstrip('/')
    allowed=set(scope.get('profiles',{}))
    known={'chickadee',*(f'chickadee-{size}-{os}' for size in ('small','medium') for os in ('rocky-102','ubuntu-2404','ubuntu-2604'))}
    runners=[];seen=set()
    for queue in queues:
        if not isinstance(queue,dict):raise ValueError('invalid runner status queue')
        if queue.get('github_url','').lower().rstrip('/')!=expected:continue
        label=queue['label'];allocated=queue['allocated_vms'];credentialed=queue['credentialed_vms']
        if label not in allowed or label not in known or label in seen or len(seen)>=32:raise ValueError('unauthorized or duplicate runner label')
        seen.add(label)
        if type(allocated) is not int or not 0<=allocated<=512 or type(credentialed) is not int or not 0<=credentialed<=allocated:raise ValueError('invalid runner counts')
        if allocated:runners.append({'label':label,'allocated':allocated,'credentialed':credentialed})
    return {'live_at':at.isoformat(),'runners':sorted(runners,key=lambda entry:entry['label'])}


def collect_account_usage(config, records, jwt, api, now, status=None):
    scopes=config.get('scopes') or {'primary':config}
    if not isinstance(scopes,dict) or len(scopes)>64:raise ValueError('too many usage scopes')
    result={};invalid=set()
    for scope in scopes.values():
        try:
            snapshot=account_usage_scope(scope,records,jwt,api,now)
            try:snapshot.update(runner_activity(status,scope,now))
            except Exception:
                print('runner activity unavailable; usage snapshot retained',file=__import__('sys').stderr)
            key=(snapshot['installation_id'],snapshot['account_type'],snapshot['account_id'] if snapshot['account_type']=='Organization' else snapshot['repository_ids'][0])
            if key in result:invalid.add(key)
            else:result[key]=snapshot
        except Exception:
            # Private metadata, tokens and upstream error bodies never enter journals.
            print('account usage ACL unavailable; omitted from replacement snapshot',file=__import__('sys').stderr)
    return [value for key,value in result.items() if key not in invalid]


def authenticated_request(entry):
    """Only private-admin site records with verified identity can enter auto policy."""
    if entry.get('authenticated') is not True:return False
    if not isinstance(entry.get('id'),str) or not entry['id']:return False
    for value in (entry.get('user',{}).get('id'),entry.get('account',{}).get('id'),entry.get('repository',{}).get('id'),entry.get('installation_id')):
        if type(value)!=int or value<=0:return False
    account=entry['account'];repo=entry['repository']
    if account.get('type')=='User':return account['id']==entry['user']['id']
    if account.get('type')=='Organization':return (repo.get('permissions') or {}).get('admin') is True
    return False


def trusted_site_entry(entry, site_available, config):
    if not authenticated_request(entry):return False
    if site_available:return True
    # Cached provenance may maintain existing scopes, never provision a new one.
    scope=(config.get('scopes') or {}).get(scope_name(entry))
    url='https://github.com/'+(entry['account']['login'] if entry['account']['type']=='Organization' else entry['repository']['full_name'])
    return bool(scope and scope.get('app_installation_id')==entry['installation_id'] and scope.get('github_url','').lower().rstrip('/')==url.lower().rstrip('/'))


def approved_request(entry, policy, profile_catalog=None, authenticated=False):
    approval=policy.get('approved_users',{}).get(str(entry['user']['id']))
    if approval is None and policy.get('auto_approve_authenticated') is True and authenticated and authenticated_request(entry):
        approval={'auto_queues':True,'max_vms':1}
    if approval is None:return None
    desired=entry.get('desired_state') or 'active'
    if desired not in ('active','paused','disconnected'):raise ValueError('invalid desired state')
    requested=entry.get('queues') or ['chickadee']
    if approval.get('auto_queues',False):
        if approval['auto_queues'] is not True or profile_catalog is None:raise ValueError('automatic queue policy requires the configured host catalog')
        allowed=list(profile_catalog)
    else:allowed=approval.get('queues',['chickadee'])
    queues=['chickadee']+list(dict.fromkeys(q for q in requested if q!='chickadee' and q in allowed))
    return dict(entry,queues=queues,max_vms=approval.get('max_vms',1))


def scope_name(entry):
    return ('org-'+str(entry['account']['id']) if entry['account']['type']=='Organization' else 'repo-'+str(entry['repository']['id']))


def validate_scope_merge(previous, entry):
    if previous is not None and (previous['user']['id']!=entry['user']['id'] or previous['installation_id']!=entry['installation_id'] or previous['repository']['id']!=entry['repository']['id']):
        raise ValueError('conflicting scope owners or repositories require operator review')


def merge_site_requests(entries, policy, catalog, site_available, original):
    updates=[]
    requests_by_scope={scope_name(e):e for e in policy.get('managed_requests',[])}
    authenticated_scopes=set()
    # Applied records win over pending conflicts regardless of listing order.
    for entry in sorted(entries,key=lambda e:not bool(e.get('enabled_queues'))):
        trusted=trusted_site_entry(entry,site_available,original)
        if approved_request(entry,policy,catalog,authenticated=trusted) is None:continue
        name=scope_name(entry);previous=requests_by_scope.get(name)
        try:
            validate_scope_merge(previous,entry)
        except ValueError:
            # Keep the established scope and let unrelated enrollments progress.
            if entry.get('id'):
                updates.append({'id':entry['id'],'status':'error','enabled_queues':[],
                    'message':'This organization already has a different repository or requester configured. Only one repository per organization is supported; contact the operator to replace it.'})
            continue
        merged=dict(entry)
        if previous is not None and not merged.get('workflow_path') and previous['repository']['id']==entry['repository']['id'] and previous.get('workflow_path'):
            merged['workflow_path']=previous['workflow_path']
        requests_by_scope[name]=merged
        if trusted:authenticated_scopes.add(name)
    return list(requests_by_scope.values()),authenticated_scopes,updates


def workflow_access(entry, policy, authenticated=False):
    mode=policy.get('repository_workflow_access',{}).get(str(entry['repository']['id']),'workflow')
    if authenticated and authenticated_request(entry) and policy.get('auto_repository_workflows') is True and entry['account']['type']=='Organization':mode='repository'
    if mode not in ('workflow','repository'):raise ValueError('invalid operator workflow access policy')
    if mode=='repository' and (entry['account']['type']!='Organization' or (not entry['repository'].get('private') and not (authenticated and policy.get('auto_repository_workflows') is True))):
        raise ValueError('repository-only workflow approval requires an exact private organization repository')
    return mode


class WorkflowPathRequired(ValueError):
    pass


def workflow_for_request(entry, policy, config):
    """Use an explicit org workflow; only existing scopes retain old policy defaults."""
    provided=entry.get('workflow_path')
    if entry['account']['type']!='Organization':
        return '.github/workflows/chickadee.yml' # Personal scopes do not use runner groups.
    if not provided:
        url=('https://github.com/'+entry['account']['login']).lower().rstrip('/')
        scopes=config.get('scopes') or {'primary':config}
        legacy=any(scope.get('github_url','').lower().rstrip('/')==url and scope.get('app_installation_id')==entry['installation_id'] for scope in scopes.values())
        if not legacy:
            raise WorkflowPathRequired('Enter the main-branch workflow file path to finish organization setup.')
        provided=policy.get('workflow','.github/workflows/ci.yaml')
    if not isinstance(provided,str) or not re.fullmatch(r'\.github/workflows/[A-Za-z0-9_-]+\.ya?ml',provided):
        raise WorkflowPathRequired('Use a workflow file path such as .github/workflows/ci.yml; the runner group allows its main branch.')
    return provided


def replace_config_bytes(path, encoded, ownership):
    fd,name=tempfile.mkstemp(prefix='.config-',dir=path.parent)
    try:
        with os.fdopen(fd,'wb') as file:file.write(encoded);file.flush();os.fsync(file.fileno())
        os.chown(name,ownership.st_uid,ownership.st_gid);os.chmod(name,ownership.st_mode&0o777)
        os.replace(name,path)
    finally:
        pathlib.Path(name).unlink(missing_ok=True)


class LiveReloadRejected(RuntimeError):
    pass

class LiveReloadUncertain(RuntimeError):
    pass


def wait_reload_ack(runtime, digest, started):
    import datetime
    deadline=time.monotonic()+150
    while True:
        try:
            acknowledgement=json.loads((runtime/'reload.json').read_text())
            updated=datetime.datetime.fromisoformat(acknowledgement['updated_at'].replace('Z','+00:00')).timestamp()
            if acknowledgement.get('config_sha256')==digest and updated>=started:
                if acknowledgement.get('status')=='applied':return
                if acknowledgement.get('status')=='rejected':raise LiveReloadRejected('controller rejected live scope update')
        except (OSError,ValueError,KeyError):pass
        if time.monotonic()>deadline:raise LiveReloadUncertain('controller did not acknowledge live scope update')
        time.sleep(1)


def install_live_config(candidate, path):
    """Install scope changes without stopping job VMs; require controller confirmation."""
    old_bytes=path.read_bytes();ownership=path.stat()
    runtime=pathlib.Path(json.loads(old_bytes)['state_dir'])
    encoded=(json.dumps(candidate,indent=2)+'\n').encode()
    digest=hashlib.sha256(encoded).hexdigest()
    backup=path.with_name(path.name+'.before-managed-update');backup.write_bytes(old_bytes);backup.chmod(0o600)
    started=time.time()
    replace_config_bytes(path,encoded,ownership)
    try:
        subprocess.run(['systemctl','kill','--kill-whom=main','--signal=SIGHUP','chickadee'],check=True)
        wait_reload_ack(runtime,digest,started)
    except LiveReloadRejected:
        # A rejected acknowledgement proves the actor retained its previous state.
        replace_config_bytes(path,old_bytes,ownership)
        raise
    except Exception:
        # A lost ACK is ambiguous: synchronize the actor back to the restored disk.
        replace_config_bytes(path,old_bytes,ownership)
        restored=time.time()
        try:
            subprocess.run(['systemctl','kill','--kill-whom=main','--signal=SIGHUP','chickadee'],check=True)
            wait_reload_ack(runtime,hashlib.sha256(old_bytes).hexdigest(),restored)
        except Exception as error:
            marker=runtime/'admission-uncertain.json'
            fd=os.open(marker,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
            with os.fdopen(fd,'w') as file:json.dump({'reason':'Live update and rollback were not acknowledged; operator must verify runtime before clearing this admission stop.'},file);file.write('\n');file.flush();os.fsync(file.fileno())
            raise LiveReloadUncertain('runtime rollback not acknowledged; further admission stopped') from error
        raise LiveReloadUncertain('live update was not acknowledged; previous runtime restored')


def install_config(candidate,path,quarantine,live_reload=False):
    encoded=json.dumps(candidate,indent=2)+'\n'
    with tempfile.TemporaryDirectory(prefix='chickadee-config-') as tmp:
        stage=pathlib.Path(tmp)/'config.json';stage.write_text(encoded);stage.chmod(0o600)
        subprocess.run(['/usr/local/bin/chickadee','-config',str(stage),'-check'],check=True)
        # Drain main process only. Never terminate a job for onboarding changes.
        state=subprocess.check_output(['systemctl','show','chickadee','-p','ActiveState','--value'],text=True).strip()
        if state=='activating':raise RuntimeError('controller still starting; retry later')
        active=state=='active'
        if active and live_reload and not quarantine:
            install_live_config(candidate,path)
            return
        if active:
            subprocess.run(['systemctl','kill','--kill-whom=main','--signal=SIGUSR1','chickadee'],check=True)
            deadline=time.monotonic()+candidate['job_timeout_seconds']+120
            while subprocess.run(['systemctl','is-active','--quiet','chickadee']).returncode==0:
                if time.monotonic()>deadline:raise RuntimeError('drain timed out; config not installed')
                time.sleep(2)
        # Journal recovery for a revoked installation is impossible until access
        # returns. Preserve those intents privately rather than losing ownership.
        runtime=pathlib.Path(candidate['state_dir'])
        if list((runtime/'vms').iterdir()):raise RuntimeError('VMs remain after drain')
        if quarantine:
            target=runtime/'revoked-records';target.mkdir(mode=0o700,exist_ok=True)
            for file in (runtime/'records').glob('*.json'):
                record=json.loads(file.read_text())
                if record['github_url'].lower().rstrip('/') in quarantine:
                    destination=target/file.name
                    if destination.exists():raise RuntimeError('quarantine record already exists')
                    os.rename(file,destination)
        backup=path.with_name(path.name+'.before-managed-update');backup.write_bytes(path.read_bytes());backup.chmod(0o600)
        old=path.stat()
        # stage is on /tmp: copy to the target filesystem before atomic replace.
        fd,name=tempfile.mkstemp(prefix='.config-',dir=path.parent)
        with os.fdopen(fd,'w') as f:f.write(encoded);f.flush();os.fsync(f.fileno())
        os.chown(name,old.st_uid,old.st_gid);os.chmod(name,old.st_mode&0o777);os.replace(name,path)
        subprocess.run(['systemctl','reset-failed','chickadee'],check=True)
        start=time.time()
        subprocess.run(['systemctl','start','chickadee'],check=True)
        deadline=time.monotonic()+300
        while True:
            try:
                ready=json.loads((runtime/'status.json').read_text())
                import datetime
                updated=datetime.datetime.fromisoformat(ready['updated_at'].replace('Z','+00:00')).timestamp()
                if updated>=start and not ready['draining']:break
            except (OSError,ValueError,KeyError):pass
            if time.monotonic()>deadline:raise RuntimeError('new config has not reached ready; no activation acknowledged')
            time.sleep(2)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config',default='/etc/chickadee/config.json')
    parser.add_argument('--policy',required=True)
    parser.add_argument('--site-host',required=True)
    parser.add_argument('--ssh-key',required=True)
    parser.add_argument('--known-hosts',required=True)
    parser.add_argument('--state-dir',default='/var/lib/chickadee-managed')
    args=parser.parse_args()
    if os.geteuid()!=0:raise ValueError('run as operator root; not the controller UID')
    runtime_lock=pathlib.Path('/run/chickadee-managed');runtime_lock.mkdir(mode=0o700,exist_ok=True)
    lock=open(runtime_lock/'lock','w');fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
    policy_path=pathlib.Path(args.policy)
    if not policy_path.is_file() or policy_path.stat().st_mode&0o077:raise ValueError('policy must be private')
    policy=json.loads(policy_path.read_text());path=pathlib.Path(args.config);original=json.loads(path.read_text());candidate=copy.deepcopy(original)
    ssh=['ssh','-i',args.ssh_key,'-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+args.known_hosts,'-o','ConnectTimeout=10','root@'+args.site_host]
    def admin(endpoint,body=None):
        command='sudo -u chickadee-web curl --fail --silent --show-error --max-time 15 --unix-socket /var/lib/chickadee-web/admin.sock '
        if body is not None:command+='-H "Content-Type: application/json" --data-binary @- '
        command+='http://localhost/'+endpoint
        result=subprocess.run([*ssh,command],input=None if body is None else json.dumps(body).encode(),stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=30,check=True)
        if len(result.stdout)>2<<20:raise ValueError('site response too large')
        return json.loads(result.stdout) if result.stdout else None
    state=pathlib.Path(args.state_dir);state.mkdir(mode=0o700,exist_ok=True)
    if state.is_symlink() or state.stat().st_uid!=0 or state.stat().st_mode&0o077:raise ValueError('managed state must be root owned and private')
    site_available=True
    try:
        entries=admin('enrollments') or []
        fd,cache=tempfile.mkstemp(prefix='.requests-',dir=state)
        with os.fdopen(fd,'w') as file:json.dump(entries,file);file.flush();os.fsync(file.fileno())
        os.replace(cache,state/'requests.json')
    except (subprocess.SubprocessError,OSError):
        site_available=False
        entries=json.loads((state/'requests.json').read_text()) if (state/'requests.json').exists() else []
    if not isinstance(entries,list) or len(entries)>500:raise ValueError('invalid site request list')
    if (pathlib.Path(original['state_dir'])/'admission-uncertain.json').exists():
        raise LiveReloadUncertain('admission stopped until operator verifies runtime')
    updates=[];quarantine=set()
    # Managed scope seeds allow revocation checks before a user has signed in.
    catalog=original.get('profiles') or original.get('scopes',{}).get('primary',{}).get('profiles',{})
    requests,authenticated_scopes,merge_updates=merge_site_requests(entries,policy,catalog,site_available,original)
    updates.extend(merge_updates)
    jwt=admit.setup.app_jwt(original['app_client_id'],pathlib.Path(original['app_key_file']))
    verified={}
    for entry in requests:
        request=approved_request(entry,policy,catalog,authenticated=scope_name(entry) in authenticated_scopes)
        if request is None:continue
        name=scope_name(entry)
        # Never let an enrollment mutate the operator's primary scope.
        url='https://github.com/'+(entry['account']['login'] if entry['account']['type']=='Organization' else entry['repository']['full_name'])
        primary=candidate.get('scopes',{}).get('primary',{}).get('github_url',candidate.get('github_url',''))
        if url.lower().rstrip('/')==primary.lower().rstrip('/'):
            if entry.get('id'):updates.append({'id':entry['id'],'status':'active','enabled_queues':['chickadee'],'message':'Operator-owned scope is active; additional queues require explicit approval.'})
            continue
        if name in verified:raise ValueError('multiple requests target one scope; operator reconciliation required')
        verified[name]=True
        desired=entry.get('desired_state') or 'active';status='pending';enabled=[];message='';applied_workflow='';applied_access='workflow'
        revoked=False;admin_missing=False
        try:
            lookup_installation=True
            installation=admit.setup.api('/app/installations/'+str(entry['installation_id']),token=jwt)
            lookup_installation=False
            admit.validate_installation_identity(installation,entry)
            required='organization_self_hosted_runners' if entry['account']['type']=='Organization' else 'administration'
            revoked=bool(installation.get('suspended_at')) or installation['permissions'].get(required)!='write'
            if not revoked:
                token=admit.setup.api('/app/installations/'+str(entry['installation_id'])+'/access_tokens','POST',jwt,{'permissions':{required:'write','metadata':'read'}})['token']
                found=False
                for page in range(1,11):
                    listing=admit.setup.api('/installation/repositories?per_page=100&page='+str(page),token=token)
                    found=any(r['id']==entry['repository']['id'] and r['full_name']==entry['repository']['full_name'] for r in listing['repositories'])
                    if found or len(listing['repositories'])<100:break
                if not found and len(listing['repositories'])==100:raise RuntimeError('repository verification exceeded page limit')
                revoked=not found
                if not revoked and name in authenticated_scopes and entry['account']['type']=='Organization':
                    admin_missing=not admit.current_repository_admin(entry,token)
                    revoked=admin_missing
        except RuntimeError as err:
            # A 404 from App-authenticated installation lookup confirms removal.
            if lookup_installation and str(err)=='GitHub setup API returned HTTP 404':revoked=True
            else:raise
        if revoked or desired!='active':
            old=candidate.get('scopes',{}).get(name)
            if revoked and old:
                candidate['scopes'].pop(name);quarantine.add(old['github_url'].lower().rstrip('/'))
            elif old:old['disabled']=True
            status='permission-required' if revoked else desired
            message=('The signup user must currently have admin access to the selected repository; restore access and retry.' if admin_missing else 'GitHub access removed, suspended, or awaiting permission approval.') if revoked else 'New assignments stopped; running jobs finished before applying this state.'
        else:
            try:
                mode=workflow_access(request,policy,authenticated=name in authenticated_scopes)
                workflow='.github/workflows/chickadee.yml' if mode=='repository' else workflow_for_request(request,policy,original)
            except WorkflowPathRequired as error:
                # One incomplete org setup must not block all other beta users.
                if entry.get('id'):
                    updates.append({'id':entry['id'],'status':'pending','enabled_queues':[],'message':str(error)})
                continue
            with tempfile.TemporaryDirectory(prefix='chickadee-admission-') as tmp:
                config=pathlib.Path(tmp)/'config.json';config.write_text(json.dumps(candidate));config.chmod(0o600)
                req=pathlib.Path(tmp)/'request.json';req.write_text(json.dumps(request));req.chmod(0o600)
                output=pathlib.Path(tmp)/'output.json'
                command=['python3',str(pathlib.Path(__file__).with_name('admit-installation.py')),'--config',str(config),'--request',str(req),'--output',str(output),'--trusted-workflows','--workflow',workflow]
                if name in authenticated_scopes:command.append('--require-current-admin')
                if mode=='repository':
                    command.append('--repository-only')
                    if not request['repository'].get('private'):command.append('--allow-public-repository')
                try:
                    subprocess.run(command,check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
                except subprocess.CalledProcessError:
                    if entry.get('id'):updates.append({'id':entry['id'],'status':'error','enabled_queues':[],
                        'message':'Runner setup could not be verified. Existing controller configuration was preserved; contact the operator to resolve setup.'})
                    continue
                candidate=json.loads(output.read_text())
            scope=candidate['scopes'][name]
            # Policy defines exact enabled queues, not a permanent union.
            scope['profiles']={q:p for q,p in scope['profiles'].items() if q in request['queues']}
            scope['max_vms']=request['max_vms']
            for profile in scope['profiles'].values():profile['max_vms']=min(profile['max_vms'],request['max_vms'])
            scope.pop('disabled',None)
            enabled=list(scope['profiles']);status='active';applied_workflow='' if mode=='repository' else workflow;applied_access=mode
            if set(entry.get('queues') or ['chickadee'])-set(enabled):message='Additional queue requests are awaiting operator approval.'
        if entry.get('id'):updates.append({'id':entry['id'],'status':status,'enabled_queues':enabled,'enabled_workflow_path':applied_workflow,'enabled_workflow_access':applied_access,'message':message})
        elif status=='active':updates.append({'id':'','status':status,'enabled_queues':enabled,'message':message,'_import':dict(entry,queues=request['queues'],enabled_queues=enabled,status='active',scope=('organization' if entry['account']['type']=='Organization' else 'repository'))})
    if candidate!=original:
        if site_available:
            for update in updates:
                if update['status']!='active' or not update.get('id'):continue
                old_entry=next(e for e in entries if e.get('id')==update['id'])
                name=scope_name(old_entry)
                if candidate.get('scopes',{}).get(name)==original.get('scopes',{}).get(name):continue
                admin('status',{'id':update['id'],'status':'approved','enabled_queues':list(original.get('scopes',{}).get(name,{}).get('profiles',{})),
                     'message':'Approved; activating selected queues in the shared fleet.' if policy.get('live_reload') is True and not quarantine else 'Approved; provisioning the selected queues after running jobs finish.'})
        install_config(candidate,path,quarantine,live_reload=policy.get('live_reload',False) is True)
    if site_available:
        # Observability failures must not block unrelated customer admission.
        try:
            sample=fleet_telemetry(candidate['state_dir'])
            if sample is not None:admin('telemetry',sample)
        except Exception:
            print('fleet telemetry unavailable; dashboard retains last observation',file=__import__('sys').stderr)
        try:
            import datetime
            usage_path=pathlib.Path(candidate['state_dir'])/'usage.json'
            if usage_path.exists() and usage_path.stat().st_size>8<<20:raise ValueError('usage ledger exceeds limit')
            records=json.loads(usage_path.read_text()) if usage_path.exists() else []
            try:runner_status=read_runner_status(candidate['state_dir'])
            except Exception:runner_status=None
            snapshots=collect_account_usage(candidate,records,jwt,admit.setup.api,datetime.datetime.now(datetime.timezone.utc),runner_status)
            admin('account-usage',snapshots)
        except Exception:
            # A failed read must not leave a previously authorized ACL snapshot active.
            try:admin('account-usage',[])
            except Exception:pass  # Site freshness expiry remains the final fail-closed bound.
            print('account usage unavailable; dashboard snapshot cleared or expires',file=__import__('sys').stderr)
        for update in updates:
            seed=update.pop('_import',None)
            if seed is not None:
                imported=admin('import-enrollment',seed)
                update['id']=imported['id']
                entry=seed
            else:entry=next(e for e in entries if e.get('id')==update['id'])
            url='https://github.com/'+(entry['account']['login'] if entry['account']['type']=='Organization' else entry['repository']['full_name'])
            usage_path=pathlib.Path(candidate['state_dir'])/'usage.json'
            records=json.loads(usage_path.read_text()) if usage_path.exists() else []
            import datetime
            today=datetime.datetime.now(datetime.timezone.utc).replace(hour=0,minute=0,second=0,microsecond=0)
            days=[]
            for offset in range(-6,1):
                start=today+datetime.timedelta(days=offset);end=start+datetime.timedelta(days=1)
                point={'date':start.date().isoformat(),'vm_seconds':0,'vms':0}
                for record in records:
                    if record['github_url'].lower().rstrip('/')!=url.lower().rstrip('/'):continue
                    reserved=datetime.datetime.fromisoformat(record['reserved_at'].replace('Z','+00:00'));completed=datetime.datetime.fromisoformat(record['completed_at'].replace('Z','+00:00'))
                    point['vm_seconds']+=max(0,(min(completed,end)-max(reserved,start)).total_seconds())
                    if start<=completed<end:point['vms']+=1
                days.append(point)
            update['usage']=days
            if update['status']=='active':
                try:applied_status=read_runner_status(candidate['state_dir'])
                except Exception:applied_status=None
                wanted=list(update.get('enabled_queues',[]))
                available=initialized_queues(applied_status,url,wanted,datetime.datetime.now(datetime.timezone.utc))
                update['enabled_queues']=available
                if set(available)!=set(wanted):
                    update['status']='pending'
                    update['message']='Selected queue listeners are initializing or retrying GitHub access. Existing running jobs are preserved.'
            admin('status',update)
    print('managed reconciliation completed; requests='+str(len(updates)))

if __name__=='__main__':
    try:main()
    except Exception:
        # Upstream bodies, SSH output and request metadata stay out of journals.
        print('managed reconciliation failed; reviewed config remains or update requires operator recovery',file=__import__('sys').stderr)
        raise SystemExit(1)
