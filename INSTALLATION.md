# Kubernetes installation

forgejo-arc runs ephemeral Forgejo Actions runners on Kubernetes. **You can create
multiple runner pools, such as `debian-small` and `debian-large`, with independent
CPU/memory budgets, concurrency limits, images, and node placement.** Workflows
select a pool through its `spec.labels`, using `runs-on`.

This guide covers:

1. [Requirements and cluster preparation](#1-requirements-and-cluster-preparation)
2. [Credentials and Secrets](#2-credentials-and-secrets)
3. [Helm installation](#3-helm-installation)
4. [Small and large runner pools](#4-small-and-large-runner-pools)
5. [Workflow examples and verification](#5-workflow-examples-and-verification)
6. [Argo CD installation](#6-argo-cd-installation)
7. [Upgrades, rotation, and removal](#7-upgrades-rotation-and-removal)
8. [Troubleshooting](#8-troubleshooting)

Examples use namespace/context `forgejo-arc`, Forgejo server
`https://git.shailesh.live`, and repository `shailesh/forgejo-arc`. Substitute your
own values consistently. Run shell examples in **Bash**, from the repository root.

## 1. Requirements and cluster preparation

- Kubernetes **1.33+**; validation uses **1.34.5**. Docker mode uses native sidecars
  and requires nodes/admission policy that permit privileged containers.
- Forgejo **v16** runner-management APIs. The test fixture uses **16.0.5**; the
  Minikube bootstrap also completed a real build against **16.0.3**.
- Runner **13.0.0** (the default runner image).
- Helm with OCI support (Helm 3.8+ or Helm 4), `kubectl`, and Docker CLI for the
  registry-credential bootstrap below.
- Network/DNS access from the controller and worker pods to Forgejo and from
  workers to job/action/image registries and dependency download sites.
- Cluster permissions to install a CRD, ClusterRole, ClusterRoleBinding, and
  namespaced controller resources.

The published chart is:

```text
oci://git.shailesh.live/shailesh/charts/forgejo-arc
```

Chart **0.1.1** defaults to controller image
`git.shailesh.live/shailesh/forgejo-arc:c5b1a70` and includes the opt-in
`dockerSocket` setting used for image builds.

For a new local cluster with capacity for the example tiers:

```sh
minikube start -p forgejo-arc --driver=docker \
  --kubernetes-version=v1.34.5 --cpus=8 --memory=16384
```

For an existing cluster, select its context instead. Check its allocatable
capacity before choosing pool requests, limits, and concurrency:

```sh
export KUBE_CONTEXT=forgejo-arc
export ARC_NAMESPACE=forgejo-arc
export ARC_RELEASE=forgejo-arc-controller
export CHART_VERSION=0.1.1
export ARC_CHART=oci://git.shailesh.live/shailesh/charts/forgejo-arc

kubectl --context "$KUBE_CONTEXT" get nodes
kubectl --context "$KUBE_CONTEXT" create namespace "$ARC_NAMESPACE" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -
```

Install **one controller installation per cluster**. Use `replicaCount` for
multiple replicas of that installation; leader election chooses one active
controller. A controller watches pools across namespaces. Each pool's Secrets
must be in that pool's namespace.

On the existing development Minikube profile, the old raw-manifest Deployment
`forgejo-arc` has been scaled to zero and the Helm release is
`forgejo-arc-controller`. Keep the old Deployment scaled to zero when using the
Helm-managed controller.

## 2. Credentials and Secrets

These credentials serve different consumers:

| Name | Location / type | Contents | Purpose / permissions |
| --- | --- | --- | --- |
| `forgejo-live-credentials` | Pool namespace; `Opaque` Secret | `token` | Controller PAT for polling jobs and registering/deleting runners; scope permissions below |
| `forgejo-registry` | Controller/worker namespace; `kubernetes.io/dockerconfigjson` Secret | `.dockerconfigjson` | Kubernetes image pulls; PAT with `read:package` and access to the package |
| `forgejo-arc-chart-repository` | `argocd`; repository Secret | `type`, `url`, `enableOCI`, `username`, `password` | Argo CD downloads the OCI chart; PAT with `read:package` |
| `forgejo-arc-git-repository` | `argocd`; repository Secret | `type`, `url`, `username`, `password` | Argo CD reads private pool manifests from Git; PAT with `read:repository` |
| `REGISTRY_TOKEN` | Forgejo repository Actions secret | PAT value | Workflow publishes container images; `write:package` for the package owner |

The controller generates a separate, owned Secret with ephemeral credentials for
each worker. You do not create runner registration tokens manually.

### 2.1 Controller API token

Create a PAT in Forgejo's user settings. Both its permission scope and the
account's access to the target scope matter:

| `spec.scope.type` | `spec.scope.name` | PAT permission | Account access |
| --- | --- | --- | --- |
| `Repository` | `owner/repository` | `write:repository` | Repository owner |
| `Organization` | Organization name | `write:organization` | Organization owner |
| `User` | Omit | `write:user` | Authenticated user's repositories |
| `Instance` | Omit | `write:admin` | Instance administrator |

For the examples, create the Secret using the repository owner's token:

```sh
read -rsp 'Forgejo controller PAT: ' FORGEJO_PAT
printf '\n'
printf '%s' "$FORGEJO_PAT" |
  kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" create secret generic \
    forgejo-live-credentials --from-file=token=/dev/stdin --dry-run=client -o yaml |
  kubectl --context "$KUBE_CONTEXT" apply -f -
unset FORGEJO_PAT
```

`spec.credentialsSecret: forgejo-live-credentials` refers to this Secret's
**`token` key**, not its entire JSON/YAML representation. Pools in the same
namespace can share the Secret if their scopes permit the same PAT.

### 2.2 Chart and image pull credentials

Skip registry login/Secrets for public packages and omit `imagePullSecrets` from
the controller values in that case. For private packages, the following uses a
temporary Docker config containing only this registry's credentials:

```sh
read -rp 'Forgejo registry username: ' REGISTRY_USER
read -rsp 'Forgejo package-read PAT: ' REGISTRY_READ_TOKEN
printf '\n'

printf '%s' "$REGISTRY_READ_TOKEN" |
  helm registry login git.shailesh.live \
    --username "$REGISTRY_USER" --password-stdin

REGISTRY_CONFIG_DIR="$(mktemp -d)"
printf '%s' "$REGISTRY_READ_TOKEN" |
  docker --config "$REGISTRY_CONFIG_DIR" login git.shailesh.live \
    --username "$REGISTRY_USER" --password-stdin
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" create secret generic \
  forgejo-registry --type=kubernetes.io/dockerconfigjson \
  --from-file=".dockerconfigjson=$REGISTRY_CONFIG_DIR/config.json" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -
rm -rf "$REGISTRY_CONFIG_DIR"
unset REGISTRY_READ_TOKEN REGISTRY_CONFIG_DIR REGISTRY_USER
```

Helm's local login lets **your CLI** download the chart. The Kubernetes Secret
lets **kubelet** pull private images. Argo CD needs its own repository credentials
in section 6; it cannot use your local Helm login.

The chart's `imagePullSecrets` applies to the **controller** pod. A pool's
`spec.imagePullSecrets` applies to its Kubernetes **runner and DinD** images.
Neither authenticates private `jobImage`, workflow `container:`, service images,
or build base images pulled inside DinD. Use workflow container/service
`credentials` for private job/service images, and registry login steps for
build/push operations. The default pool job image in this guide is public.

### 2.3 Workflow publishing secret

In Forgejo, open the repository's **Settings → Actions → Secrets** and add
`REGISTRY_TOKEN` with a PAT that has `write:package`. The publishing workflow
logs in as `${{ github.repository_owner }}`, so this token must belong to the
package owner (`shailesh` here). A read-only image-pull token cannot publish.

For GitOps-managed Secrets, use your existing External Secrets, Sealed Secrets,
or SOPS integration to produce the same names, types, and keys. Supply Argo CD's
repository credentials before it attempts to fetch the chart/Git source; keep
the decryption or external-store bootstrap outside the repository that depends
on those credentials. Commit encrypted definitions or references, not PATs,
plaintext Secret manifests, or `.dockerconfigjson` contents.

## 3. Helm installation

The chart installs the controller, RBAC, CRD, and optional metrics Service.
RunnerPools and credential Secrets are separate resources.

Create `arc-values.yaml`:

```yaml
replicaCount: 1
image:
  repository: git.shailesh.live/shailesh/forgejo-arc
  tag: c5b1a70
  pullPolicy: IfNotPresent
imagePullSecrets:
  - name: forgejo-registry
resources:
  requests: {cpu: 100m, memory: 128Mi}
  limits: {memory: 256Mi}
metrics:
  enabled: true
```

These `resources` size the controller only; runner sizing is in section 4.
Apply the CRD explicitly, then install the published chart:

```sh
helm show crds "$ARC_CHART" --version "$CHART_VERSION" |
  kubectl --context "$KUBE_CONTEXT" apply -f -
helm upgrade --install "$ARC_RELEASE" "$ARC_CHART" \
  --version "$CHART_VERSION" --kube-context "$KUBE_CONTEXT" \
  --namespace "$ARC_NAMESPACE" --create-namespace \
  --values arc-values.yaml --wait --timeout 5m
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" rollout status \
  deployment/"$ARC_RELEASE"
```

Helm installs chart CRDs on an initial install but does not upgrade them on
`helm upgrade`. Keep the explicit CRD apply step for every version upgrade.

## 4. Small and large runner pools

### 4.1 What each resource setting controls

| Setting | What it controls |
| --- | --- |
| Helm `resources` | Controller container, shared by all pools |
| Pool `spec.resources` | Runner container; also the workflow process in **Host** mode |
| Pool `spec.dockerResources` | DinD sidecar in **Docker** mode, which runs the nested workflow/build/service containers |
| Pool `spec.maxRunners` | Maximum starting + running workers for this pool |
| Pool `spec.nodeSelector`, `tolerations`, `affinity` | Worker pod placement; independent of controller placement |

In Docker mode, increasing only `spec.resources` gives the runner more resources;
size `dockerResources` for CPU/memory-heavy builds. These are Kubernetes
container budgets, not separate limits for each nested service/build container.
Requests reserve scheduling capacity; CPU limits throttle and memory limits can
cause OOM termination. Budget the runner **plus** the native DinD sidecar per
worker, multiplied by `maxRunners`, plus the controller and cluster workloads.
The controller creates worker pods; it does not provision additional nodes.

The following tiers use Debian Bookworm with Node already installed for
JavaScript actions. A plain `debian:bookworm` image needs Node installed before
JavaScript actions can run. Other tools, including Docker CLI, can be installed
by the workflow or included in a custom job image.

| Pool / label | Max workers | Runner request → limit | DinD request → limit | Docker socket |
| --- | --- | --- | --- | --- |
| `debian-small` | 2 | 100m / 128Mi → 500m / 256Mi | 250m / 512Mi → 1 CPU / 1Gi | Disabled |
| `debian-large` | 1 | 100m / 128Mi → 500m / 512Mi | 2 CPU / 2Gi → 4 CPU / 8Gi | Enabled for image builds |

### 4.2 Pool manifests

The complete manifests are also in
[`config/samples/debian-pools.yaml`](config/samples/debian-pools.yaml):

```yaml
apiVersion: arc.forgejo.org/v1alpha1
kind: RunnerPool
metadata:
  name: debian-small
  namespace: forgejo-arc
spec:
  forgejoURL: https://git.shailesh.live
  scope: {type: Repository, name: shailesh/forgejo-arc}
  credentialsSecret: forgejo-live-credentials
  labels: [debian-small]
  maxRunners: 2
  mode: Docker
  jobImage: docker.io/library/node:22-bookworm-slim
  dockerImage: docker.io/library/docker:28.5.2-dind
  dockerSocket: false
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {cpu: 500m, memory: 256Mi}
  dockerResources:
    requests: {cpu: 250m, memory: 512Mi}
    limits: {cpu: '1', memory: 1Gi}
  startupTimeoutSeconds: 600
  jobTimeoutSeconds: 1800
  pollIntervalSeconds: 5
---
apiVersion: arc.forgejo.org/v1alpha1
kind: RunnerPool
metadata:
  name: debian-large
  namespace: forgejo-arc
spec:
  forgejoURL: https://git.shailesh.live
  scope: {type: Repository, name: shailesh/forgejo-arc}
  credentialsSecret: forgejo-live-credentials
  labels: [debian-large]
  maxRunners: 1
  mode: Docker
  jobImage: docker.io/library/node:22-bookworm-slim
  dockerImage: docker.io/library/docker:28.5.2-dind
  dockerSocket: true
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {cpu: 500m, memory: 512Mi}
  dockerResources:
    requests: {cpu: '2', memory: 2Gi}
    limits: {cpu: '4', memory: 8Gi}
  startupTimeoutSeconds: 600
  jobTimeoutSeconds: 3600
  pollIntervalSeconds: 5
```

Adjust `forgejoURL`, `scope`, namespace, and resource sizing before applying:

```sh
kubectl --context "$KUBE_CONTEXT" apply -f config/samples/debian-pools.yaml
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" wait \
  --for=condition=Ready runnerpool/debian-small runnerpool/debian-large --timeout=120s
```

Both pools can watch the same repository. Their distinct labels select the
resource tier. All labels in a workflow's `runs-on` array must be present in a
pool; an array is **AND**, not a fallback list. `metadata.name` is a Kubernetes
name; `spec.labels` is what the workflow matches. If different-size pools share
the same matching label, either may claim a job; there is no size preference.

Use the same canonical Forgejo URL in overlapping pools. `forgejoURL` and `scope`
are immutable; create a replacement pool to change them. Resource/image/label
changes apply to new workers; existing workers retain their configuration
snapshot. Set `maxRunners: 0` to pause new workers while existing jobs drain.

`dockerSocket: true` mounts that worker's disposable DinD socket into workflow
containers at `/var/run/docker.sock`. It does not mount the Kubernetes node's
socket. It is useful for Docker/Buildx commands and is disabled by default.

To place a pool on dedicated build nodes, label those nodes and add matching
settings to that pool, for example:

```yaml
spec:
  nodeSelector:
    ci.example.com/size: large
  tolerations:
    - key: ci.example.com/dedicated
      operator: Equal
      value: runners
      effect: NoSchedule
```

The selector requires an existing matching node label; a toleration permits a
tainted node but does not select it. Leave these fields unset on a single-node
Minikube cluster unless you have configured the corresponding labels/taints.

## 5. Workflow examples and verification

Enable Actions in the Forgejo repository. A minimal smoke workflow is:

```yaml
name: Runner tiers
on:
  workflow_dispatch:
jobs:
  small:
    runs-on: debian-small
    steps:
      - run: |
          cat /etc/os-release
          node --version
          echo 'Running in the small pool'
  large:
    runs-on: debian-large
    steps:
      - run: |
          cat /etc/os-release
          node --version
          test -S /var/run/docker.sock
          echo 'Running in the large pool with a worker Docker socket'
```

Save this under `.forgejo/workflows/`, commit/push, and run it manually in Forgejo
Actions. An idle pool has zero runner pods and may have no registered runner
visible in Forgejo; the controller polls waiting jobs and creates registrations
on demand.

To build and publish container images on the large tier, use
[`.forgejo/workflows/publish.yaml`](.forgejo/workflows/publish.yaml) and set its
`runs-on` to `debian-large`. Its steps install Docker CLI, set up Buildx, log in
using `REGISTRY_TOKEN`, and push commit/latest image tags. The current repository
workflow uses `arc-minikube`, served by
[`config/samples/minikube-build-pool.yaml`](config/samples/minikube-build-pool.yaml).
Creating the Debian pools alone does not change that workflow's routing.

Inspect workers and status:

```sh
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" get runnerpools,jobs,pods
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" describe runnerpool debian-large
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" logs deploy/"$ARC_RELEASE"
# While a worker exists, substitute its pod name:
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" logs <worker-pod> -c runner
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" logs <worker-pod> -c docker
```

Check **Forgejo's workflow result** for success. A completed Kubernetes Job only
means the one-job runner finished; the workflow itself may have failed. After a
run, the controller removes worker Jobs, owned credentials, and registrations.
The metrics Service is `${ARC_RELEASE}-metrics` on port 8080 when enabled.

## 6. Argo CD installation

Use an existing, cluster-capable Argo CD installation with multi-source
Application support (2.6+; use a currently supported release). The example
targets the cluster where Argo CD runs. For a remote cluster, register it with
Argo CD and change `destination.server` or use `destination.name`.

Choose Helm CLI management **or** Argo CD management for the controller. For an
existing Helm deployment, see the handover instructions below before enabling
Argo CD sync.

### 6.1 Bootstrap Secrets before the Application

1. Create the destination namespace and `forgejo-live-credentials` as in section 2.
2. Create `forgejo-registry` there if controller/worker images are private.
3. Create the following repository Secrets in the **Argo CD namespace**, usually
   `argocd`, if chart/Git access requires authentication.

The required Secret shapes are shown below. Replace placeholder values through
your secret-management system, or use the interactive command after the example;
do not commit filled-in plaintext credentials.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: forgejo-arc-chart-repository
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
type: Opaque
stringData:
  name: forgejo-arc-charts
  type: helm
  url: git.shailesh.live/shailesh/charts
  enableOCI: 'true'
  username: shailesh
  password: REPLACE_WITH_PACKAGE_READ_PAT
---
apiVersion: v1
kind: Secret
metadata:
  name: forgejo-arc-git-repository
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
type: Opaque
stringData:
  type: git
  url: https://git.shailesh.live/shailesh/forgejo-arc.git
  username: shailesh
  password: REPLACE_WITH_REPOSITORY_READ_PAT
```

For an interactive bootstrap without writing credential files (Python 3 required):

```sh
export ARGOCD_NAMESPACE=argocd
python3 - <<'PY'
import getpass
import json
import os
import subprocess

username = input('Forgejo username: ')
repositories = [
    ('forgejo-arc-chart-repository', {
        'name': 'forgejo-arc-charts', 'type': 'helm',
        'url': 'git.shailesh.live/shailesh/charts', 'enableOCI': 'true',
    }, 'Package-read PAT: '),
    ('forgejo-arc-git-repository', {
        'type': 'git', 'url': 'https://git.shailesh.live/shailesh/forgejo-arc.git',
    }, 'Repository-read PAT: '),
]
for name, fields, prompt in repositories:
    fields.update(username=username, password=getpass.getpass(prompt))
    secret = {
        'apiVersion': 'v1', 'kind': 'Secret', 'type': 'Opaque',
        'metadata': {
            'name': name, 'namespace': os.environ['ARGOCD_NAMESPACE'],
            'labels': {'argocd.argoproj.io/secret-type': 'repository'},
        },
        'stringData': fields,
    }
    subprocess.run(
        ['kubectl', '--context', os.environ['KUBE_CONTEXT'], 'apply', '-f', '-'],
        input=json.dumps(secret), text=True, check=True,
    )
PY
```

The chart repository URL in Argo CD deliberately omits **`oci://`**. This example
uses Argo CD's Helm OCI integration (`type: helm`, `enableOCI: 'true'`), with
`chart: forgejo-arc` separately specified in the Application.

If Argo CD manages another cluster, `KUBE_CONTEXT` for the repository-secret and
Application commands must point to the **Argo CD control cluster**; the pool and
image-pull Secrets must be created in the **destination cluster**.

### 6.2 Application: OCI chart plus Git-managed pools

The following combines the published chart with **only**
`config/samples/debian-pools.yaml` from Git. The include filter avoids deploying
the other local test/bootstrap samples. For your installation, put edited pool
manifests in your own GitOps repository/path and update the Git source and its
credential Secret URL. Commit and push that file before syncing.

Save as `forgejo-arc-application.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: forgejo-arc-controller
  namespace: argocd
spec:
  project: default
  sources:
    - repoURL: git.shailesh.live/shailesh/charts
      chart: forgejo-arc
      targetRevision: 0.1.1
      helm:
        releaseName: forgejo-arc-controller
        skipCrds: false
        values: |
          replicaCount: 1
          image:
            repository: git.shailesh.live/shailesh/forgejo-arc
            tag: c5b1a70
          imagePullSecrets:
            - name: forgejo-registry
          resources:
            requests: {cpu: 100m, memory: 128Mi}
            limits: {memory: 256Mi}
          metrics:
            enabled: true
    - repoURL: https://git.shailesh.live/shailesh/forgejo-arc.git
      targetRevision: main
      path: config/samples
      directory:
        include: debian-pools.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: forgejo-arc
  syncPolicy:
    automated:
      prune: false
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
```

Keep the Application name and Helm release name equal; this chart uses
`app.kubernetes.io/instance` selectors. The `default` AppProject must permit both
source URLs, the destination namespace, and the chart's cluster-scoped CRD/RBAC
resources. If using a restricted AppProject, grant those explicitly. Argo CD's
Kubernetes credentials also need permission to create those resources.

Apply and sync using a CLI logged in to your Argo CD instance:

```sh
kubectl --context "$KUBE_CONTEXT" apply -f forgejo-arc-application.yaml
argocd app sync forgejo-arc-controller
argocd app wait forgejo-arc-controller --sync --health --timeout 300
# Run against the destination cluster:
kubectl --context "$KUBE_CONTEXT" -n forgejo-arc wait \
  --for=condition=Ready runnerpool/debian-small runnerpool/debian-large --timeout=120s
```

Argo CD renders Helm templates and manages their lifecycle itself; a fresh Argo
CD deployment does not create a Helm CLI release visible in `helm list`.
With `skipCrds: false`, Argo CD includes the CRD in its managed resources and
syncs it before custom resources. Unlike Helm CLI upgrades, chart CRD updates
are applied during Argo CD sync. Server-side apply avoids the client-side
last-applied annotation size limit for large CRDs. Check RunnerPool `Ready`
explicitly; Argo CD health alone does not confirm Forgejo authentication.

Automatic pruning is disabled in this example so removing a pool from Git does
not silently remove its dependencies along with the controller. To remove a
pool, first remove it from Git, sync, then delete just that RunnerPool and wait
for its finalizer to finish. Keep the controller and credentials running during
this process. If your Application is generated by an ApplicationSet, make
configuration changes in its template/source.

### 6.3 Handover from the existing Helm release

For the current Minikube deployment, retain namespace `forgejo-arc`, Application
name/release name `forgejo-arc-controller`, chart version, and current values:

1. Inspect `helm get values forgejo-arc-controller -n forgejo-arc
   --kube-context forgejo-arc --all` and match them in the Application.
2. Bootstrap the Argo CD repository Secrets and review the rendered diff before
   sync. Avoid creating a second controller under a new release name.
3. Sync the Application to adopt the same named Kubernetes resources. Keep all
   existing pool/registry Secrets available. The existing `forgejo-arc-build`
   pool remains in the cluster; the example Application manages the two Debian
   pools, so add the bootstrap pool to your GitOps source if you also want it
   managed there.
4. Use Argo CD as the sole configuration owner afterward. Existing Helm release
   history may remain, but do not run `helm upgrade`, rollback, or uninstall on
   the old release after handover; uninstall would delete resources now managed
   by Argo CD.

## 7. Upgrades, rotation, and removal

### Upgrade or resize

- **Helm CLI:** choose the new chart version, apply that version's CRDs, then run
  `helm upgrade --install` with your values. Pin controller image tags rather than
  relying on `latest`.
- **Argo CD:** update chart `targetRevision` and any image override in Git, then
  sync. The example pins both; changing just the chart version leaves an explicit
  image override in place.
- **Pools:** edit and apply/sync `resources`, `dockerResources`, `maxRunners`, or
  placement settings. Existing workers keep their snapshots. Scaling the
  controller Deployment does not change worker concurrency.

### Rotate credentials

Update `forgejo-live-credentials` in place (repeat the token Secret command).
The controller reads the Secret during reconciliation; existing registered
workers use their own ephemeral credentials. Replace registry/repository Secret
data and the Forgejo Actions `REGISTRY_TOKEN` independently as needed. For
GitOps-managed Secrets, update the external store or encrypted source so the
secret controller does not restore the previous value.

### Remove the installation

1. For Argo CD, disable automatic sync/recreation in its Git source or parent
   ApplicationSet first. Remove pools from the desired manifests, then sync
   without pruning controller/Secrets.
2. Delete **all pools managed by this cluster's controller**, keeping it and
   their API Secrets available. Deletion stops new work and drains running jobs.

   ```sh
   kubectl --context "$KUBE_CONTEXT" get runnerpools -A
   kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" delete \
     runnerpool debian-small debian-large --wait=false
   kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" wait \
     --for=delete runnerpool/debian-small runnerpool/debian-large --timeout=3600s
   ```

   Repeat for any other pools, including `forgejo-arc-build` if present.
3. For a Helm-managed installation, uninstall after pools have finished draining:

   ```sh
   helm uninstall "$ARC_RELEASE" --kube-context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE"
   ```

   For Argo CD, after all pools are gone, delete the Application with cascading
   deletion to remove its controller resources:

   ```sh
   argocd app delete forgejo-arc-controller --cascade
   ```

   Argo CD manages the chart CRD and can delete it during cascading deletion;
   ensure no RunnerPools remain anywhere in the cluster. Helm CLI retains CRDs
   on uninstall. Avoid deleting the CRD or namespace as a shortcut to draining.
4. Delete unused API/pull Secrets last. Argo CD repository Secrets are in the
   Argo CD namespace; retain any shared with other Applications.

## 8. Troubleshooting

| Symptom | Checks |
| --- | --- |
| Pool `Ready=False` | `kubectl describe runnerpool`; verify URL, Secret namespace/key, PAT scope and account ownership; check controller logs |
| Workflow stays waiting | Exact `runs-on`/pool label match, scope/repository, `maxRunners > 0`, cluster/controller running; inspect `Degraded` and `exhaustedJobs` |
| Worker stays Pending | Node capacity, sum of runner + sidecar requests, selectors/taints, namespace quotas; inspect pod Events |
| Privileged pod rejected | Namespace admission policy and node support for Docker-mode DinD |
| Kubernetes `ImagePullBackOff` | Correct controller/pool `imagePullSecrets` and image tag; nested Docker pulls use separate workflow credentials |
| Docker command missing | Install Docker CLI in the job image/workflow; `dockerSocket` supplies a daemon connection, not a CLI binary |
| Docker daemon inaccessible in steps | Use Docker mode and `dockerSocket: true` on chart/controller 0.1.1+; inspect sidecar logs and `/var/run/docker.sock` |
| Build OOM or slow | Size `dockerResources`, including nested services/BuildKit; CPU limits throttle; inspect pod/container termination status |
| Argo CD cannot fetch chart | Repository Secret in `argocd`, `type: helm`, `enableOCI: 'true'`, correct username/PAT, URL without `oci://` |
| Argo CD cannot create pools | Chart CRD included, cluster RBAC allowed by AppProject and Argo CD credentials, destination Secrets present |
| Deletion is stuck | Keep controller and API credentials available; restore Forgejo connectivity and let running jobs finish before removing finalizers/dependencies |

The actual bootstrap procedure for this repository is also recorded in
[`docs/minikube-bootstrap.md`](docs/minikube-bootstrap.md). Argo CD reference:
[Helm OCI sources](https://argo-cd.readthedocs.io/en/stable/user-guide/helm/),
[private repositories](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/).
