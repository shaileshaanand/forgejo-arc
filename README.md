# forgejo-arc

A small Kubernetes controller for ephemeral Forgejo Actions runners. Targets
Forgejo **16.0.5**, Runner **13.0.0**, and Kubernetes **1.34**.

A `RunnerPool` polls the Forgejo runner jobs API and launches a single-use
Kubernetes Job for each matching job attempt. The runner uses `one-job --handle`
to request that exact attempt. Credentials are unique to each worker. Idle pools
have no runner pods.

## Development

```sh
make generate test build
make minikube
make image
minikube -p forgejo-arc image load forgejo-arc:dev
make deploy
```

Create a same-namespace Secret named `forgejo-credentials` with a `token` key
containing a Forgejo personal access token. Repository scope requires repository
owner access and a token with `write:repository` permission. Configure and apply
`config/samples/runnerpool.yaml`. Use `runs-on: arc-host` in workflows.

Host-mode steps run inside the runner container. Choose a runner image containing
the tools your workflows need. The stock image supports shell and Git workflows.

Pool deletion drains running workers before removing external registrations.
Keep the credentials Secret available until deletion completes.

## Minikube end-to-end test

Use the dedicated `forgejo-arc` kubectl context. The fixture is a real Forgejo
16.0.5 server with SQLite and a PVC, not a mocked API.

```sh
kubectl --context forgejo-arc apply -f test/e2e/forgejo.yaml
kubectl --context forgejo-arc -n forgejo-arc rollout status deploy/forgejo
# Keep this running in another terminal:
kubectl --context forgejo-arc -n forgejo-arc port-forward svc/forgejo 3000:3000

python3 test/e2e/setup.py
kubectl --context forgejo-arc apply -f config/samples/runnerpool.yaml
python3 test/e2e/smoke.py
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
