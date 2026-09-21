#!/usr/bin/env python3
"""Delete a running pod and verify it is not replayed as a new runner."""
import json
import time
from smoke import api, push, wait_clean, K, run

wait_clean()
commit = push('slow.yaml')
deadline = time.monotonic() + 120
while time.monotonic() < deadline:
    active = api('/repos/arc/ci/actions/runners/jobs') or []
    if any(j['status'] == 'running' for j in active):
        break
    time.sleep(1)
else:
    raise AssertionError('Runner did not acquire work')
pods = json.loads(run(K + ['get', 'pods', '-l', 'arc.forgejo.org/pool-uid', '-o', 'json'], capture_output=True).stdout)['items']
assert len(pods) == 1
original = {r['id'] for r in (api('/repos/arc/ci/actions/runners') or [])}
run(K + ['delete', 'pod', pods[0]['metadata']['name'], '--wait=true', '--timeout=90s'])
deadline = time.monotonic() + 180
while time.monotonic() < deadline:
    assert {r['id'] for r in (api('/repos/arc/ci/actions/runners') or [])} <= original, 'Pod loss created another runner for an in-flight attempt'
    current = next(r for r in api('/repos/arc/ci/actions/runs')['workflow_runs'] if r['commit_sha'] == commit)
    if current['status'] in ('cancelled', 'failure', 'success'):
        print('Deleted-pod workflow result:', current['status'])
        break
    time.sleep(2)
else:
    raise AssertionError('Deleted-pod workflow did not reach a terminal state')
wait_clean()
