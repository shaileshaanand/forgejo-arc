#!/usr/bin/env python3
"""Deleting a pool drains its running workflow before completing deletion."""
import json
import time
from smoke import api, push, wait_result, wait_clean, K, run

wait_clean()
resource = {'apiVersion':'arc.forgejo.org/v1alpha1','kind':'RunnerPool','metadata':{'name':'drain-test'},'spec':{
    'forgejoURL':'http://forgejo.forgejo-arc.svc.cluster.local:3000','scope':{'type':'Repository','name':'arc/ci'},
    'credentialsSecret':'forgejo-credentials','labels':['arc-drain'],'pollIntervalSeconds':2,'jobTimeoutSeconds':120}}
run(K + ['apply','-f','-'], input=json.dumps(resource))
try:
    commit = push('drain.yaml')
    for _ in range(120):
        if any(j['status']=='running' and j['runs_on']==['arc-drain'] for j in (api('/repos/arc/ci/actions/runners/jobs') or [])):
            break
        time.sleep(1)
    else:
        raise AssertionError('Drain worker did not acquire work')
    run(K + ['delete','runnerpool','drain-test','--wait=false'])
    wait_result(commit)
    run(K + ['wait','--for=delete','runnerpool/drain-test','--timeout=120s'])
    wait_clean()
finally:
    run(K + ['delete','runnerpool','drain-test','--ignore-not-found','--wait=false'],capture_output=True)
print('Pool deletion drained its running workflow successfully')
