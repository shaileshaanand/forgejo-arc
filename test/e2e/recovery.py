#!/usr/bin/env python3
"""Exercise restart, outage, startup failure, cancellation and workflow failure."""
import json
import time
from smoke import api, push, wait_result, wait_clean, K, run


def wait_for(check, timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(1)
    raise AssertionError('Condition was not met before timeout')


def workers():
    return json.loads(run(K + ['get', 'jobs', '-l', 'arc.forgejo.org/pool-uid', '-o', 'json'], capture_output=True).stdout)['items']


def pool_patch(spec):
    run(K + ['patch', 'runnerpool', 'example', '--type=merge', '-p', json.dumps({'spec':spec})], capture_output=True)


wait_clean(180)
commit = push('slow.yaml')
wait_for(lambda: any(j['status'] == 'running' for j in (api('/repos/arc/ci/actions/runners/jobs') or [])))
original = {w['metadata']['uid'] for w in workers()}
run(K + ['rollout', 'restart', 'deploy/forgejo-arc'], capture_output=True)
run(K + ['rollout', 'status', 'deploy/forgejo-arc', '--timeout=120s'], capture_output=True)
assert {w['metadata']['uid'] for w in workers()} == original
# Remove service endpoints temporarily; existing runner and controller must recover.
try:
    run(K + ['patch', 'svc', 'forgejo', '--type=merge', '-p', '{"spec":{"selector":{"app":"outage"}}}'], capture_output=True)
    def unavailable():
        pool = json.loads(run(K + ['get', 'runnerpool', 'example', '-o', 'json'], capture_output=True).stdout)
        return any(c['type'] == 'Ready' and c['status'] == 'False' for c in pool.get('status', {}).get('conditions', []))
    wait_for(unavailable, 60)
    assert {w['metadata']['uid'] for w in workers()} == original
finally:
    run(K + ['patch', 'svc', 'forgejo', '--type=merge', '-p', '{"spec":{"selector":{"app":"forgejo"}}}'], capture_output=True)
wait_result(commit)
wait_clean()
print('Restart and Forgejo outage preserved the original worker')

pool_patch({'runnerImage':'invalid.example.test/runner:missing', 'startupTimeoutSeconds':15})
try:
    commit = push()
    wait_for(workers)
    pool_patch({'runnerImage':'data.forgejo.org/forgejo/runner:13.0.0'})
    wait_result(commit)
    wait_clean()
finally:
    pool_patch({'runnerImage':'data.forgejo.org/forgejo/runner:13.0.0', 'startupTimeoutSeconds':180})
print('Unstartable worker was replaced and the waiting attempt completed')

commit = push('slow.yaml')
wait_for(lambda: any(j['status'] == 'running' for j in (api('/repos/arc/ci/actions/runners/jobs') or [])))
current = next(r for r in api('/repos/arc/ci/actions/runs')['workflow_runs'] if r['commit_sha'] == commit)
api(f"/repos/arc/ci/actions/runs/{current['id']}/cancel", 'POST')
wait_result(commit, 'cancelled')
wait_clean()
print('Cancelled workflow cleaned up')

wait_result(push('failure.yaml'), 'failure')
wait_clean()
print('Workflow failure was reported by Forgejo and did not trigger a replay')
