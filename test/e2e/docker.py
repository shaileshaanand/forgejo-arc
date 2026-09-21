#!/usr/bin/env python3
from smoke import push, wait_result, wait_clean, K, ROOT, run

run(K + ['apply', '-f', str(ROOT / 'config/samples/docker-pool.yaml')])
wait_result(push('docker.yaml'), limit=900)
wait_clean()
print('Container job, service container, Docker action and default Node image passed')
