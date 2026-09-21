#!/usr/bin/env python3
import json
import time
from smoke import api, push, wait_result, wait_clean, K, run

run(K + ['patch', 'runnerpool', 'example', '--type=merge', '-p', '{"spec":{"maxRunners":2}}'])
commit = push('matrix.yaml')
peak = 0
seen = set()
deadline = time.monotonic() + 300
while time.monotonic() < deadline:
    workers = json.loads(run(K + ['get', 'jobs', '-l', 'arc.forgejo.org/pool-uid', '-o', 'json'], capture_output=True).stdout)['items']
    active = [job for job in workers if not any(c['type'] in ('Complete', 'Failed') and c['status'] == 'True' for c in job.get('status', {}).get('conditions', []))]
    assert len(active) <= 2, 'Concurrency limit exceeded'
    peak = max(peak, len(active))
    seen.update(job['metadata']['name'] for job in workers)
    matching = [r for r in api('/repos/arc/ci/actions/runs')['workflow_runs'] if r['commit_sha'] == commit]
    if matching and matching[0]['status'] in ('success', 'failure', 'cancelled'):
        break
    time.sleep(1)
wait_result(commit)
assert peak == 2, f'Expected scaling to two workers, observed {peak}'
assert len(seen) == 5, f'Expected five distinct workers, observed {len(seen)}'
wait_clean()
print('Autoscaling: five jobs, peak two workers, returned to zero')
