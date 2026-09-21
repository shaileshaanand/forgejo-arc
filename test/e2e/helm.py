#!/usr/bin/env python3
"""Install/upgrade a two-replica chart, run work, inspect metrics, uninstall."""
import json
import os
import subprocess
import time
import urllib.request
from smoke import push, wait_result, wait_clean, K, ROOT, run

profile = os.environ.get('PROFILE', 'forgejo-arc')
wait_clean()
deployment = json.loads(run(K + ['get','deploy','forgejo-arc','-o','json'], capture_output=True).stdout)
image = deployment['spec']['template']['spec']['containers'][0]['image']
repository, tag = image.rsplit(':', 1)
helm = ['helm','--kube-context',profile]
release = 'arc-chart-test'
chart = str(ROOT / 'charts/forgejo-arc')
values = ['--namespace','forgejo-arc','--set','image.repository='+repository,'--set-string','image.tag='+tag,'--wait','--timeout','180s']
forward = None
try:
    run(K + ['scale','deploy/forgejo-arc','--replicas=0'])
    run(helm + ['install',release,chart] + values)
    run(helm + ['upgrade',release,chart,'--set','replicaCount=2'] + values)
    wait_result(push())
    wait_clean()
    # Scrape each replica directly: only the elected leader exports pool samples.
    pods = json.loads(run(K + ['get','pods','-l','app.kubernetes.io/instance='+release,'-o','json'],capture_output=True).stdout)['items']
    samples = ''
    for pod in pods:
        forward = subprocess.Popen(K + ['port-forward','pod/'+pod['metadata']['name'],'33001:8080'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        for _ in range(40):
            try:
                with urllib.request.urlopen('http://127.0.0.1:33001/metrics',timeout=2) as response:
                    samples += response.read().decode()
                break
            except OSError:
                time.sleep(0.25)
        forward.terminate(); forward.wait(timeout=10); forward = None
    assert 'forgejo_arc_workers_started_total{' in samples
    assert 'forgejo_arc_pool_ready{' in samples
    assert 'forgejo_arc_active_runners{' in samples
    print('Chart install, two-replica upgrade, workflow and metrics passed')
finally:
    if forward:
        forward.terminate(); forward.wait(timeout=10)
    try:
        run(helm + ['uninstall',release,'--namespace','forgejo-arc','--wait','--timeout','180s'])
    finally:
        run(K + ['scale','deploy/forgejo-arc','--replicas=1'])
        run(K + ['rollout','status','deploy/forgejo-arc','--timeout=120s'])
print('Chart uninstall passed; development controller restored')
