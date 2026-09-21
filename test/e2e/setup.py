#!/usr/bin/env python3
"""Bootstrap the dedicated Minikube fixture. Secrets stay in .local/ and Kubernetes."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
LOCAL = ROOT / '.local'
HOST = os.environ.get('FORGEJO_TEST_URL', 'http://127.0.0.1:3000')
K = ['kubectl', '--context', os.environ.get('PROFILE', 'forgejo-arc'), '-n', 'forgejo-arc']


def run(args, **kw):
    return subprocess.run(args, check=True, text=True, **kw)


def main():
    LOCAL.mkdir(mode=0o700, exist_ok=True)
    admin = K + ['exec', 'deploy/forgejo', '--', 'su', 'git', '-c']
    if not (LOCAL / 'token').exists():
        password = secrets.token_hex(24)
        result = subprocess.run(admin + [f'forgejo admin user create --username arc --password {password} --email arc@example.test --admin --must-change-password=false'], capture_output=True, text=True)
        if result.returncode and 'already exists' not in result.stdout + result.stderr:
            raise RuntimeError('Could not create Forgejo test user: ' + result.stderr)
        token = run(admin + ['forgejo admin user generate-access-token --username arc --token-name arc-e2e --scopes all --raw'], capture_output=True).stdout.strip().splitlines()[-1]
        (LOCAL / 'token').write_text(token)
        (LOCAL / 'token').chmod(0o600)
    token = (LOCAL / 'token').read_text().strip()
    run(K + ['apply', '-f', '-'], input=json.dumps({'apiVersion':'v1','kind':'Secret','metadata':{'name':'forgejo-credentials'},'stringData':{'token':token}}), capture_output=True)
    # fj v0.6 attempts to inspect the current Git branch even with an explicit host.
    with tempfile.TemporaryDirectory(prefix='arc-fj-') as cwd:
        run(['fj', 'auth', 'add-token', '-H', HOST], input=token, capture_output=True, cwd=cwd)
        if subprocess.run(['fj', 'repo', 'view', '-H', HOST, 'arc/ci'], capture_output=True, cwd=cwd).returncode:
            run(['fj', 'repo', 'create', '-H', HOST, 'ci'], cwd=cwd)
    print('Forgejo fixture ready; credentials stored in .local/token')


if __name__ == '__main__':
    main()
