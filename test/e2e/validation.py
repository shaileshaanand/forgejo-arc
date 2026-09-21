#!/usr/bin/env python3
"""Validate idle status and deletion of a misconfigured pool against the real API."""
import json
import subprocess
import time
from smoke import K, run


def create(name, credentials):
    run(K + ['apply','-f','-'],input=json.dumps({'apiVersion':'arc.forgejo.org/v1alpha1','kind':'RunnerPool','metadata':{'name':name},'spec':{
        'forgejoURL':'http://forgejo.forgejo-arc.svc.cluster.local:3000','scope':{'type':'Repository','name':'arc/ci'},
        'credentialsSecret':credentials,'labels':['unused-validation-label'],'maxRunners':0,'pollIntervalSeconds':2}}))


def status(name, expected):
    for _ in range(60):
        pool=json.loads(run(K + ['get','runnerpool',name,'-o','json'],capture_output=True).stdout)
        if any(c['type']=='Ready' and c['status']==expected for c in pool.get('status',{}).get('conditions',[])):
            assert pool['status']['activeRunners']==0
            assert pool['status']['waitingJobs']==0
            assert pool['status']['exhaustedJobs']==0
            return pool
        time.sleep(1)
    raise AssertionError('Pool status was not initialized')


create('idle-validation','forgejo-credentials')
try:
    status('idle-validation','True')
    result=subprocess.run(K + ['patch','runnerpool','idle-validation','--type=merge','-p','{"spec":{"scope":{"name":"arc/other"}}}'],capture_output=True)
    assert result.returncode!=0, 'Scope mutation should be rejected by the CRD'
finally:
    run(K + ['delete','runnerpool','idle-validation','--timeout=60s'])

create('invalid-validation','missing-credentials')
try:
    pool=status('invalid-validation','False')
    assert not pool['metadata'].get('finalizers'), 'Unprovisioned invalid pool must remain deletable'
finally:
    run(K + ['delete','runnerpool','invalid-validation','--timeout=60s'])
print('Idle status, immutable scope and invalid-pool deletion passed')
