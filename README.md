# forgejo-arc

A small Kubernetes controller for ephemeral Forgejo Actions runners. Targets
Forgejo **16.0.5**, Runner **13.0.0**, and Kubernetes **1.34**.

A `RunnerPool` polls the Forgejo runner jobs API and launches a single-use
Kubernetes Job for each matching job attempt. The runner uses `one-job --handle`
to request that exact attempt. Credentials are unique to each worker. Idle pools
have no runner pods.

`spec.maxRunners` defaults to 1 and bounds starting and running workers together.
Set it to 0 to pause new work; existing workers drain. Increasing it enables
parallel matrix jobs. Workflow reruns use new attempt handles and new workers.

Install one controller per cluster (multiple replicas share its leader-election
lock). Reconciliation is serialized and uses uncached Kubernetes reads to claim
attempts across pools/namespaces. Overlapping pools can share demand: an existing
worker claim prevents another pool from launching the same attempt. Use the same
canonical Forgejo URL in all pools for a server.

## Development

### Published controller image

The [container workflow](.forgejo/workflows/publish.yaml) runs on `dind` and
publishes to `git.shailesh.live/shailesh/forgejo-arc` on pushes to `main` or manual
runs against `main`. Each build publishes the seven-character commit SHA and
`latest` tags. Pull requests build the image without logging in or pushing.

Add a repository Actions secret named `REGISTRY_TOKEN` containing a personal
access token for `shailesh` with `write:package` permission. The workflow logs in
as the repository owner. The `dind` runner must provide a working Docker CLI and
daemon; the workflow sets up Buildx. The OCI source label associates the image
with this repository in Forgejo's package registry.

```sh
docker pull git.shailesh.live/shailesh/forgejo-arc:latest
```

For bootstrapping through Helm, set `image.repository` to
`git.shailesh.live/shailesh/forgejo-arc` and `image.tag` to the published commit
tag. Configure the chart's `imagePullSecrets` if the package is private.

### Local builds

```sh
make generate test build
make minikube
IMAGE="forgejo-arc:local-$(date +%s)"
make image IMAGE="$IMAGE"
minikube -p forgejo-arc image load --daemon "$IMAGE"
make deploy
kubectl --context forgejo-arc -n forgejo-arc set image deploy/forgejo-arc "controller=$IMAGE"
```

Create a same-namespace Secret named `forgejo-credentials` with a `token` key
containing a Forgejo personal access token. Repository scope requires repository
owner access and a token with `write:repository` permission. Configure and apply
`config/samples/runnerpool.yaml`. Use `runs-on: arc-host` in workflows.

### Runner scopes

| `scope.type` | `scope.name` | Token permission and account access |
| --- | --- | --- |
| `Repository` | `owner/repository` | `write:repository`; repository owner |
| `Organization` | Organization name | `write:organization`; organization owner |
| `User` | Omit | `write:user`; authenticated user's repositories |
| `Instance` | Omit | `write:admin`; instance administrator |

Forgejo URL and scope are immutable. Create a new pool to change either. Credentials
can be rotated by updating the referenced Secret. The test fixture uses an admin
token with all scopes to exercise every endpoint; deployed pools can use the
scope-specific permissions above.

Host-mode steps run inside the runner container. Choose a runner image containing
the tools your workflows need. The stock image supports shell and Git workflows.

### Docker workflows

Set `mode: Docker` and use `config/samples/docker-pool.yaml`. Each worker gets a
private Docker-in-Docker native sidecar with an ephemeral Docker data directory.
The runner waits for `docker info` to succeed before starting. Kubernetes stops
the sidecar when the runner exits, allowing the Job to complete normally.

This supports workflow `container:`, service containers, Docker actions, and
JavaScript actions using a Node-capable `jobImage`. The Docker daemon runs
privileged and listens on pod loopback; the runner itself stays non-root. This
mode requires nodes that permit privileged pods and Kubernetes native sidecars
(stable in 1.33). Configure `dockerResources` to budget the daemon and nested
workflow containers. No node Docker socket or host directory is mounted.

Pool deletion drains running workers before removing external registrations.
Keep the credentials Secret available until deletion completes.

Workers have external-registration finalizers. Provisioning recovers a lost
registration response by deleting the unusable registration before replacing it.
The controller also sweeps registrations bearing its pool UID when their worker
record is gone. Startup is bounded by `startupTimeoutSeconds` (default 180), with
up to `maxAttempts` (default 3) while Forgejo still reports the attempt as waiting.
Failed attempt records are retained while Forgejo lists the attempt, preventing
unbounded retries. A new Forgejo workflow attempt starts with a fresh retry budget.

An API outage reports `Ready=False` and backs off; it does not remove active
workers. Cancellation/disappearance has a 30-second grace period for final logs.
`jobTimeoutSeconds` bounds each unsuspended worker's lifetime. Running workflows
are never automatically replayed by this controller: Forgejo determines their
result and handles workflow reruns. Kubernetes Job success means the runner
finished, not necessarily that the workflow passed.

Worker settings support `resources`, `dockerResources`, `nodeSelector`,
`tolerations`, `affinity`, and `imagePullSecrets`. Each worker keeps a snapshot of
its pool configuration, so later changes apply to newly created workers. Image
pull Secrets are same-namespace Kubernetes image credentials; credentials for
private images pulled by the nested Docker daemon are configured by the workflow.

## Helm installation

Build and load the controller image as above, then use Helm instead of the raw
manager manifest for a fresh installation:

```sh
helm upgrade --install forgejo-arc charts/forgejo-arc \
  --namespace forgejo-arc --create-namespace \
  --set image.repository=forgejo-arc --set-string image.tag="${IMAGE#forgejo-arc:}" \
  --wait
```

For a remote cluster, set `image.repository` and `image.tag` to an image available
to its nodes. The chart can run multiple controller replicas with one elected
leader. Install one release per cluster. CRDs are included for installation;
before upgrading the chart, apply `charts/forgejo-arc/crds/` explicitly because
Helm does not upgrade CRDs in its `crds/` directory.

Before uninstalling, delete RunnerPools and wait for draining/finalizers while
the controller and their credential Secrets are still available. Then uninstall
the Helm release. Helm retains the CRD.

### Status and metrics

`kubectl describe runnerpool NAME` shows `Ready`, `Degraded`, active runners,
waiting jobs, exhausted jobs, and provisioning/error Events. `Ready=False`
includes the API or configuration error. `Degraded=True/Reason=RetriesExhausted`
means a waiting attempt exhausted its startup retry budget. Fix the image or
scheduling settings, then increase `maxAttempts` or cancel/rerun the workflow.

The chart exposes a ClusterIP metrics service on port 8080. Prometheus metrics
include `forgejo_arc_active_runners`, `forgejo_arc_waiting_jobs`,
`forgejo_arc_exhausted_jobs`, `forgejo_arc_pool_ready`, and
`forgejo_arc_workers_started_total`, labelled by namespace and pool. Scrape each
controller pod; pool metrics are emitted by the active leader. Standard
controller-runtime reconciliation, workqueue, process, and Go metrics are also
available. Workflow results remain authoritative in Forgejo.

## Minikube end-to-end test

Use the dedicated `forgejo-arc` kubectl context. The fixture is a real Forgejo
16.0.5 server with SQLite and a PVC, not a mocked API.

`make e2e` provisions the cluster, builds a uniquely tagged image, and runs the
full suite including Helm lifecycle checks. It leaves the cluster available for
inspection. To run tests individually:

```sh
kubectl --context forgejo-arc apply -f test/e2e/forgejo.yaml
kubectl --context forgejo-arc -n forgejo-arc rollout status deploy/forgejo
# Keep this running in another terminal:
kubectl --context forgejo-arc -n forgejo-arc port-forward svc/forgejo 3000:3000

python3 test/e2e/setup.py
kubectl --context forgejo-arc apply -f config/samples/runnerpool.yaml
python3 test/e2e/smoke.py
python3 test/e2e/autoscale.py
python3 test/e2e/recovery.py
python3 test/e2e/pod_loss.py
python3 test/e2e/docker.py
python3 test/e2e/scopes.py
python3 test/e2e/drain.py
python3 test/e2e/helm.py
```

Setup requires `fj` v0.6 and creates the local test repository `arc/ci`. It stores
the test API token in gitignored `.local/token` (mode 0600) and configures `fj`
authentication for the local test instance. Smoke tests push a workflow, check
its result through the Forgejo API, then assert worker and registration cleanup.

Inspect the controller and pool with:

```sh
kubectl --context forgejo-arc -n forgejo-arc get runnerpools,jobs,pods
kubectl --context forgejo-arc -n forgejo-arc logs deploy/forgejo-arc
```

To remove the fixture, stop the port-forward, delete the dedicated Minikube
profile (`minikube delete -p forgejo-arc`), and remove `.local/` before recreating
it. The test credentials belong to that specific server database.
