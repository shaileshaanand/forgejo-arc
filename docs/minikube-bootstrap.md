# Build forgejo-arc on Minikube

The `forgejo-arc-build` RunnerPool connects to
`https://git.shailesh.live/shailesh/forgejo-arc` and serves `runs-on: arc-minikube`.
It launches at most one ephemeral worker and scales to zero between builds.

## Controller installation

Start the local profile and log in to the registry if its packages are private:

```sh
minikube start -p forgejo-arc --driver=docker --kubernetes-version=v1.34.5
helm registry login git.shailesh.live
kubectl --context forgejo-arc create namespace forgejo-arc --dry-run=client -o yaml |
  kubectl --context forgejo-arc apply -f -
```

Create a `forgejo-live-credentials` Secret in namespace `forgejo-arc` with a
`token` key containing a PAT for the repository owner with `write:repository`
permission. Keep this Secret available while the pool drains or is deleted.
For private controller images, create a `forgejo-registry` Docker registry
Secret in the same namespace with credentials for `git.shailesh.live`.

Chart `0.1.2` includes image repository
`git.shailesh.live/shailesh/forgejo-arc` and image tag `0.1.2`; its chart version
and `appVersion` are also `0.1.2`. No image override is needed. Install the OCI
chart, applying its CRDs explicitly for upgrades:

```sh
helm show crds oci://git.shailesh.live/shailesh/charts/forgejo-arc --version 0.1.2 |
  kubectl --context forgejo-arc apply -f -
helm upgrade --install forgejo-arc-controller \
  oci://git.shailesh.live/shailesh/charts/forgejo-arc \
  --version 0.1.2 --kube-context forgejo-arc \
  --namespace forgejo-arc --create-namespace \
  --set 'imagePullSecrets[0].name=forgejo-registry' --wait
kubectl --context forgejo-arc apply -f config/samples/minikube-build-pool.yaml
kubectl --context forgejo-arc -n forgejo-arc wait \
  --for=condition=Ready runnerpool/forgejo-arc-build --timeout=120s
```

This profile originally contained the raw-manifest development controller named
`forgejo-arc`. It is scaled to zero when using the Helm-managed
`forgejo-arc-controller`; run only one controller installation per cluster.

## Workflow builds

The pool uses Docker mode with `dockerSocket: true` (requires controller/chart
`0.1.1` or newer). Each worker has its own privileged Docker native sidecar,
disposable image storage, and a shared Unix socket. The runner mounts that socket
into workflow containers at `/var/run/docker.sock`. This is the worker daemon,
not the Kubernetes node's daemon. Socket access is disabled by default in other
pools.

On branch pushes, pull requests targeting `main`, and manual runs, the workflow
checks formatting, race-enabled tests, `go vet`, Helm lint/rendering, and generated
files. Git tag pushes run those checks, then a dependent image job installs Docker
CLI, bootstraps Buildx, and builds/publishes the controller with the Git tag, short
commit SHA, and `latest`. After the image push, it publishes an OCI Helm chart
with `version`, `appVersion`, and `image.tag` equal to that exact Git tag, and
`image.repository` set to `git.shailesh.live/shailesh/forgejo-arc`. It uses the
repository's `REGISTRY_TOKEN` Actions secret with `write:package` permission for
both artifacts. Only tag pushes build container images and publish charts. Use
version tags such as `v0.1.2` or `v0.1.2-rc.1`, without `+build` metadata.

```sh
kubectl --context forgejo-arc -n forgejo-arc get runnerpools,jobs,pods
kubectl --context forgejo-arc -n forgejo-arc logs deploy/forgejo-arc-controller
```

The local machine and Minikube profile must be running for jobs to execute.
Each published release chart pins its matching controller image. Subsequent tag
releases publish new images/charts without automatically upgrading the running
controller. Upgrade the Helm chart version to deploy the corresponding image;
remove any previously configured image overrides first.
