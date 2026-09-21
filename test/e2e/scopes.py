#!/usr/bin/env python3
import json
import urllib.error
from smoke import api, push, wait_result, wait_clean, K, HOST, run


def exists(path):
    try:
        api(path)
        return True
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return False
        raise


def pool(kind, name=None):
    scope = {'type':kind}
    if name:
        scope['name'] = name
    resource = {'apiVersion':'arc.forgejo.org/v1alpha1','kind':'RunnerPool','metadata':{'name':'scope-test'},'spec':{
        'forgejoURL':'http://forgejo.forgejo-arc.svc.cluster.local:3000',
        'scope':scope,'credentialsSecret':'forgejo-credentials','labels':['arc-scope'],
        'maxRunners':2,'pollIntervalSeconds':2,'jobTimeoutSeconds':300}}
    run(K + ['apply', '-f', '-'], input=json.dumps(resource))


def remove_pool():
    run(K + ['delete', 'runnerpool', 'scope-test', '--timeout=120s'])


if not exists('/orgs/arc-team'):
    run(['fj','org','create','-H',HOST,'arc-team'])
for repo in ('one', 'two'):
    if not exists('/repos/arc-team/' + repo):
        # fj v0.6 does not expose organization repository creation.
        api('/orgs/arc-team/repos', 'POST', {'name':repo,'default_branch':'main'})
pool('Organization', 'arc-team')
try:
    commits = [(repo, push('scope.yaml', 'arc-team/' + repo)) for repo in ('one','two')]
    for repo, commit in commits:
        wait_result(commit, repo_name='arc-team/' + repo)
    wait_clean()
    assert not api('/orgs/arc-team/actions/runners')
finally:
    remove_pool()
print('Organization pool ran jobs from two repositories')

for kind, route in [('User','/user'),('Instance','/admin')]:
    pool(kind)
    try:
        wait_result(push('scope.yaml'))
        wait_clean()
        assert not api(route + '/actions/runners')
    finally:
        remove_pool()
    print(kind + ' scope passed')
