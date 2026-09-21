#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
export PROFILE="${PROFILE:-forgejo-arc}"
export FORGEJO_TEST_URL="${FORGEJO_TEST_URL:-http://127.0.0.1:33000}"
PORT="${FORGEJO_TEST_URL##*:}"
IMAGE="forgejo-arc:e2e-$(date +%s)"
K=(kubectl --context "$PROFILE" -n forgejo-arc)

cleanup() {
  result=$?
  if (( result != 0 )); then
    "${K[@]}" get runnerpools,jobs,pods || true
    "${K[@]}" logs deploy/forgejo-arc --tail=80 || true
  fi
  if [[ -n "${forward_pid:-}" ]]; then kill "$forward_pid" 2>/dev/null || true; fi
  exit "$result"
}
trap cleanup EXIT

make minikube
"${K[@]}" apply -f test/e2e/forgejo.yaml
"${K[@]}" rollout status deploy/forgejo --timeout=300s
make generate test image IMAGE="$IMAGE"
minikube -p "$PROFILE" image load --daemon "$IMAGE"
make deploy
"${K[@]}" set image deploy/forgejo-arc "controller=$IMAGE"
"${K[@]}" rollout status deploy/forgejo-arc --timeout=120s
mkdir -p .local
"${K[@]}" port-forward svc/forgejo "$PORT:3000" >.local/port-forward.log 2>&1 &
forward_pid=$!
python3 - <<'PY'
import os, time, urllib.request
for _ in range(60):
    try:
        with urllib.request.urlopen(os.environ['FORGEJO_TEST_URL'] + '/api/v1/version', timeout=2):
            break
    except OSError:
        time.sleep(1)
else:
    raise SystemExit('Forgejo port-forward failed')
PY
python3 test/e2e/setup.py
"${K[@]}" apply -f config/samples/runnerpool.yaml
python3 -u test/e2e/smoke.py
python3 -u test/e2e/autoscale.py
python3 -u test/e2e/recovery.py
python3 -u test/e2e/pod_loss.py
python3 -u test/e2e/docker.py
