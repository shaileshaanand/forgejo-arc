#!/usr/bin/env python3
"""Push a real workflow and assert Forgejo result and eventual resource cleanup."""
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
import urllib.request

from setup import ROOT, LOCAL, HOST, K, run


def api(path):
    token = (LOCAL / 'token').read_text().strip()
    req = urllib.request.Request(HOST + '/api/v1' + path, headers={'Authorization': 'token ' + token})
    with urllib.request.urlopen(req, timeout=20) as response:
        return json.load(response)


def push(workflow='smoke.yaml'):
    repo = LOCAL / 'ci'
    repo.mkdir(exist_ok=True)
    if not (repo / '.git').exists():
        run(['git', 'init', '-b', 'main', str(repo)], capture_output=True)
        run(['git', '-C', str(repo), 'remote', 'add', 'origin', HOST + '/arc/ci.git'])
    workflows = repo / '.forgejo/workflows'
    workflows.mkdir(parents=True, exist_ok=True)
    for old in workflows.glob('*.yaml'):
        old.unlink()
    shutil.copy(ROOT / 'test/e2e/workflows' / workflow, workflows / workflow)
    run(['git', '-C', str(repo), 'add', '.'])
    run(['git', '-C', str(repo), '-c', 'user.name=ARC Test', '-c', 'user.email=arc@example.test', 'commit', '--allow-empty', '-m', 'Run ' + workflow], capture_output=True)
    token = (LOCAL / 'token').read_text().strip()
    auth = base64.b64encode(('arc:' + token).encode()).decode()
    env = dict(os.environ, GIT_CONFIG_COUNT='1', GIT_CONFIG_KEY_0='http.extraHeader', GIT_CONFIG_VALUE_0='Authorization: Basic ' + auth)
    run(['git', '-C', str(repo), 'push', 'origin', 'main'], env=env, capture_output=True)
    return run(['git', '-C', str(repo), 'rev-parse', 'HEAD'], capture_output=True).stdout.strip()


def wait_result(commit, expected='success', limit=300):
    deadline = time.monotonic() + limit
    while time.monotonic() < deadline:
        runs = api('/repos/arc/ci/actions/runs')['workflow_runs']
        for workflow in runs:
            if workflow['commit_sha'] == commit and workflow['status'] in ('success','failure','cancelled','skipped'):
                assert workflow['status'] == expected, workflow
                print(f"Forgejo run {workflow['id']}: {expected}")
                return workflow
        time.sleep(2)
    raise AssertionError('Workflow did not finish before deadline')


def wait_clean(limit=90):
    deadline = time.monotonic() + limit
    while time.monotonic() < deadline:
        jobs = json.loads(run(K + ['get', 'jobs', '-l', 'arc.forgejo.org/pool-uid', '-o', 'json'], capture_output=True).stdout)['items']
        credentials = json.loads(run(K + ['get', 'secrets', '-l', 'arc.forgejo.org/pool-uid', '-o', 'json'], capture_output=True).stdout)['items']
        runners = api('/repos/arc/ci/actions/runners')
        if not jobs and not credentials and not runners:
            print('No remaining workers, runner credentials, or Forgejo registrations')
            return
        time.sleep(2)
    raise AssertionError('Runner resources were not cleaned up')


if __name__ == '__main__':
    wait_result(push())
    wait_clean()
