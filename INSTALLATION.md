# Kubernetes installation

forgejo-arc runs ephemeral Forgejo Actions runners on Kubernetes. **You can create
multiple runner pools, such as `debian-small` and `debian-large`, with independent
CPU/memory budgets, concurrency limits, images, and node placement.** Workflows
select a pool through its `spec.labels`, using `runs-on`.

This guide covers:

1. [Requirements and cluster preparation](#1-requirements-and-cluster-preparation)
2. [Credentials and Secrets](#2-credentials-and-secrets)
   - [Store Kubernetes credentials with Sealed Secrets](#24-store-kubernetes-credentials-with-sealed-secrets)
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

Chart **0.1.2** includes the controller image repository and release tag:

```yaml
image:
  repository: git.shailesh.live/shailesh/forgejo-arc
  tag: 0.1.2
```

Its chart `version` and `appVersion` are also `0.1.2`. Selecting chart version
`0.1.2` therefore deploys `git.shailesh.live/shailesh/forgejo-arc:0.1.2`
automatically. **Do not add image overrides for a normal release installation.**
Use overrides only for a custom build or registry mirror. This chart includes
the opt-in `dockerSocket` setting used for image builds.

You can inspect the published defaults directly:

```sh
helm show chart oci://git.shailesh.live/shailesh/charts/forgejo-arc --version 0.1.2
helm show values oci://git.shailesh.live/shailesh/charts/forgejo-arc --version 0.1.2
```

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
export CHART_VERSION=0.1.2
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
| `REGISTRY_TOKEN` | Forgejo repository Actions secret | PAT value | Tag-release workflow publishes container images and Helm charts; `write:package` for the package owner |

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
package owner (`shailesh` here). This token publishes both the container image
and OCI Helm chart on tag pushes. A read-only image-pull token cannot publish.

For GitOps-managed Secrets, use your existing External Secrets, Sealed Secrets,
or SOPS integration to produce the same names, types, and keys. Supply Argo CD's
repository credentials before it attempts to fetch the chart/Git source; keep
the decryption or external-store bootstrap outside the repository that depends
on those credentials. Commit encrypted definitions or references, not PATs,
plaintext Secret manifests, or `.dockerconfigjson` contents.

### 2.4 Store Kubernetes credentials with Sealed Secrets

Sealed Secrets lets you commit **encrypted `SealedSecret` manifests** to Git.
Its controller decrypts them inside the target cluster and creates the ordinary
Kubernetes Secrets consumed by forgejo-arc, kubelet, and Argo CD. Existing
`credentialsSecret` and `imagePullSecrets` references do not change.

| Encrypted manifest produces | Namespace | Consumer |
| --- | --- | --- |
| `forgejo-live-credentials`, key `token` | `forgejo-arc` | forgejo-arc controller, using a repository-management PAT |
| `forgejo-registry`, key `.dockerconfigjson` | `forgejo-arc` | Kubernetes, using package-read credentials |
| `forgejo-arc-chart-repository`, including the Argo CD repository label | `argocd` | Argo CD, using package-read credentials |
| `forgejo-arc-git-repository`, including the Argo CD repository label | `argocd` | Argo CD, using repository-read credentials |

These are the same Secrets described above, managed through encryption rather
than direct `kubectl create secret` commands. A single package-read PAT can be
used in both registry-related Secrets; separate storage is needed because the
consumers expect different formats and namespaces.

**`REGISTRY_TOKEN` stays in Forgejo Actions settings.** Sealed Secrets creates
Kubernetes Secrets; it does not populate Forgejo's repository Actions secrets.
Likewise, ephemeral worker credentials remain owned and generated by forgejo-arc,
not stored in Git or managed through Sealed Secrets.

#### Install the controller and obtain its public certificate

Install the local `kubeseal` CLI from the
[official releases](https://github.com/bitnami-labs/sealed-secrets/releases), using
a version compatible with your selected controller. The Helm chart version and
controller/CLI version are different version numbers. If the cluster already
has a Sealed Secrets controller, reuse it and adjust its name/namespace below.

```sh
export SEALED_SECRETS_NAMESPACE=kube-system
export SEALED_SECRETS_CONTROLLER=sealed-secrets-controller

helm repo add sealed-secrets https://bitnami.github.io/sealed-secrets
helm repo update sealed-secrets
helm search repo sealed-secrets/sealed-secrets --versions
# Select and record a chart version from the list, then enter it here:
read -rp 'Sealed Secrets chart version: ' SEALED_SECRETS_CHART_VERSION
helm upgrade --install sealed-secrets sealed-secrets/sealed-secrets \
  --version "${SEALED_SECRETS_CHART_VERSION:?}" --kube-context "$KUBE_CONTEXT" \
  --namespace "$SEALED_SECRETS_NAMESPACE" \
  --set-string fullnameOverride="$SEALED_SECRETS_CONTROLLER" --wait

mkdir -p clusters/forgejo-arc/secrets/{workloads,bootstrap}
export SEALED_SECRETS_CERT="$PWD/clusters/forgejo-arc/sealed-secrets.pem"
kubeseal --context "$KUBE_CONTEXT" \
  --controller-namespace "$SEALED_SECRETS_NAMESPACE" \
  --controller-name "$SEALED_SECRETS_CONTROLLER" \
  --fetch-cert > "$SEALED_SECRETS_CERT"
```

The certificate is **public** and may be committed. Fetch it from the intended
cluster, refresh it when sealing new credentials, and retain the controller's
private keys separately as described below. These instructions use `strict`
scope: ciphertext is bound to the Secret's **name and namespace**, as well as
the cluster's sealing key. Renaming it or moving it to another namespace requires
resealing; copying it to a new Minikube cluster will not work without the keys.

The examples assume Argo CD and workers use the same cluster. For separate
control/destination clusters, install or reuse a Sealed Secrets controller in
each and seal the `argocd` Secrets with the **control cluster's** certificate and
the workload Secrets with the **destination cluster's** certificate.

#### Generate encrypted manifests without plaintext credential files

Use this instead of the direct Secret-creation commands in sections 2.1, 2.2,
and 6.1. It prompts for PATs, sends each Secret to `kubeseal` through stdin, and
writes only encrypted output. Python 3 and `kubeseal` must be on your PATH.
The example assumes private controller images; omit the registry Secret and its
references if all images are public. Choose the optional Argo CD credentials
only when using Argo CD with private chart/Git sources.

```sh
export ARGOCD_NAMESPACE=argocd
python3 - <<'PY'
import base64
import getpass
import json
import os
from pathlib import Path
import subprocess

root = Path('clusters/forgejo-arc/secrets')
cert = os.environ['SEALED_SECRETS_CERT']
namespace = os.environ['ARC_NAMESPACE']

def seal(name, namespace, data, directory, secret_type='Opaque', labels=None):
    metadata = {'name': name, 'namespace': namespace}
    if labels:
        metadata['labels'] = labels
    secret = {
        'apiVersion': 'v1', 'kind': 'Secret', 'type': secret_type,
        'metadata': metadata,
        'data': {k: base64.b64encode(v.encode()).decode() for k, v in data.items()},
    }
    result = subprocess.run(
        ['kubeseal', '--cert', cert, '--scope', 'strict', '--format', 'yaml'],
        input=json.dumps(secret), text=True, capture_output=True, check=True,
    )
    path = root / directory / (name + '.yaml')
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(result.stdout)
    print('Wrote encrypted manifest:', path)

seal('forgejo-live-credentials', namespace,
     {'token': getpass.getpass('Controller PAT (write:repository): ')}, 'workloads')

username = input('Forgejo registry username: ')
package_token = getpass.getpass('Package-read PAT: ')
auth = base64.b64encode((username + ':' + package_token).encode()).decode()
dockerconfig = json.dumps({'auths': {'git.shailesh.live': {'auth': auth}}})
seal('forgejo-registry', namespace, {'.dockerconfigjson': dockerconfig},
     'workloads', secret_type='kubernetes.io/dockerconfigjson')

if input('Seal Argo CD repository credentials too? [y/N] ').lower() == 'y':
    argo_namespace = os.environ['ARGOCD_NAMESPACE']
    labels = {'argocd.argoproj.io/secret-type': 'repository'}
    seal('forgejo-arc-chart-repository', argo_namespace, {
        'name': 'forgejo-arc-charts', 'type': 'helm',
        'url': 'git.shailesh.live/shailesh/charts', 'enableOCI': 'true',
        'username': username, 'password': package_token,
    }, 'bootstrap', labels=labels)
    git_username = input('Forgejo Git username: ')
    seal('forgejo-arc-git-repository', argo_namespace, {
        'type': 'git', 'url': 'https://git.shailesh.live/shailesh/forgejo-arc.git',
        'username': git_username,
        'password': getpass.getpass('Repository-read PAT: '),
    }, 'bootstrap', labels=labels)
PY
```

The generated files preserve the Secret type and keys. For Argo CD credentials,
verify that `spec.template.metadata.labels` contains
`argocd.argoproj.io/secret-type: repository`. The label must reach the **generated
Secret**, not just the SealedSecret's top-level metadata. `kubeseal` preserves
the input Secret's labels in this template.

The resulting Git layout is:

```text
clusters/forgejo-arc/
  sealed-secrets.pem                         # Public certificate
  secrets/
    bootstrap/
      forgejo-arc-chart-repository.yaml      # Encrypted, namespace argocd
      forgejo-arc-git-repository.yaml        # Encrypted, namespace argocd
    workloads/
      forgejo-live-credentials.yaml          # Encrypted, namespace forgejo-arc
      forgejo-registry.yaml                  # Encrypted, namespace forgejo-arc
```

#### Adopt existing Secrets, apply, and validate

When converting the existing Minikube installation, explicitly allow Sealed
Secrets to take ownership of the already-created Secrets. Run these commands
only for Secrets that already exist; fresh installations skip this step:

```sh
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" annotate secret \
  forgejo-live-credentials forgejo-registry \
  sealedsecrets.bitnami.com/managed=true --overwrite
# If these Argo CD repository Secrets already exist:
kubectl --context "$KUBE_CONTEXT" -n "$ARGOCD_NAMESPACE" annotate secret \
  forgejo-arc-chart-repository forgejo-arc-git-repository \
  sealedsecrets.bitnami.com/managed=true --overwrite
```

Ensure both destination namespaces exist (`forgejo-arc` from section 1 and
`argocd` from your Argo CD installation). Validate each generated encrypted file
against the controller before applying it:

```sh
for file in clusters/forgejo-arc/secrets/{bootstrap,workloads}/*.yaml; do
  [ -f "$file" ] || continue
  kubeseal --context "$KUBE_CONTEXT" \
    --controller-namespace "$SEALED_SECRETS_NAMESPACE" \
    --controller-name "$SEALED_SECRETS_CONTROLLER" --validate < "$file"
done

kubectl --context "$KUBE_CONTEXT" apply -f clusters/forgejo-arc/secrets/workloads/
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" wait \
  --for=condition=Synced sealedsecret/forgejo-live-credentials \
  sealedsecret/forgejo-registry --timeout=120s
kubectl --context "$KUBE_CONTEXT" -n "$ARC_NAMESPACE" get secret \
  forgejo-live-credentials forgejo-registry
```

If you generated Argo CD repository credentials, apply those as well:

```sh
kubectl --context "$KUBE_CONTEXT" apply -f clusters/forgejo-arc/secrets/bootstrap/
kubectl --context "$KUBE_CONTEXT" -n "$ARGOCD_NAMESPACE" wait \
  --for=condition=Synced sealedsecret/forgejo-arc-chart-repository \
  sealedsecret/forgejo-arc-git-repository --timeout=120s
```

The controller now manages the ordinary Secrets. Stop managing those same
Secrets with plaintext manifests or another secret controller. If unsealing
fails, inspect `kubectl describe sealedsecret NAME -n NAMESPACE` and the Sealed
Secrets controller logs; check the certificate, name/namespace, and ownership
of any existing Secret.

#### Argo CD bootstrap order and GitOps ownership

Use this order to avoid requiring Git credentials to fetch themselves:

1. Install Sealed Secrets independently, and create the required namespaces.
2. Generate and apply the encrypted `bootstrap/` repository credentials manually
   (or through an already-working platform/bootstrap Application).
3. Wait for their generated Secrets before Argo CD fetches the private chart or
   Git repository. Sync waves cannot fix a missing credential needed to download
   an Application's own sources.
4. Apply the encrypted workload Secrets and wait for `Synced`, then install/sync
   the forgejo-arc controller and pools using section 3 or 6.
5. Commit/push the encrypted manifests. For ongoing Argo CD management, add the
   following entry to the Application's `spec.sources` list from section 6.2:

   ```yaml
   - repoURL: https://git.shailesh.live/shailesh/forgejo-arc.git
     targetRevision: main
     path: clusters/forgejo-arc/secrets/workloads
     directory:
       recurse: false
   ```

Use the repository/path where you actually committed your cluster's encrypted
files. The existing `config/samples` source's `include: debian-pools.yaml` filter
does **not** include the Secrets directory. The AppProject and Argo CD's cluster
credentials must allow `bitnami.com/SealedSecret` resources in the destination
namespace. Keep the Sealed Secrets CRD/controller installed as a prerequisite.

Keep `bootstrap/` credentials under a separately bootstrapped platform
Application, or continue applying their encrypted manifests manually. The
forgejo-arc workload Application should not own credentials whose deletion
would prevent Argo CD from fetching its own configuration. Argo CD manages the
**SealedSecret** manifests; Sealed Secrets manages the resulting **Secret**
objects. Keep automatic pruning disabled while arranging this ownership.

#### Rotation, deletion, and key recovery

- **Rotate a PAT:** generate a replacement PAT, fetch the current public
  certificate, rerun the sealing commands with the new value, and apply/sync the
  updated encrypted file. Wait for `Synced` and verify its consumer before
  revoking the old PAT. If a PAT is shared, update every Secret that uses it.
  Controller sealing-key renewal does not rotate Forgejo PATs.
- **Delete:** a managed Secret is normally deleted with its owning SealedSecret.
  Keep the controller API SealedSecret and its generated Secret present until
  RunnerPools finish draining. Keep Argo CD repository credentials available
  until the Applications that use them have been removed.
- **Back up keys:** the public certificate cannot decrypt existing ciphertext.
  Back up all controller sealing-key Secrets after key renewal, outside Git,
  in your protected backup store. For example, first set `KEY_BACKUP_FILE` to a
  path outside the repository, then export with restrictive permissions:

  ```sh
  (umask 077
    kubectl --context "$KUBE_CONTEXT" -n "$SEALED_SECRETS_NAMESPACE" get secret \
      -l sealedsecrets.bitnami.com/sealed-secrets-key -o yaml \
      > "${KEY_BACKUP_FILE:?Set an external private-key backup path}"
  )
  ```

  This file contains **private decryption keys**, not encrypted application
  credentials. Restore it into the same controller namespace in a replacement
  cluster before replaying the SealedSecrets, then restart an already-running
  controller so it loads the restored keys:

  ```sh
  kubectl --context "$KUBE_CONTEXT" apply -f "$KEY_BACKUP_FILE"
  kubectl --context "$KUBE_CONTEXT" -n "$SEALED_SECRETS_NAMESPACE" \
    rollout restart deployment/"$SEALED_SECRETS_CONTROLLER"
  kubectl --context "$KUBE_CONTEXT" -n "$SEALED_SECRETS_NAMESPACE" \
    rollout status deployment/"$SEALED_SECRETS_CONTROLLER"
  ```

  Without the old keys, a rebuilt Minikube cluster needs new credentials sealed
  with its new certificate. See the
  [upstream Sealed Secrets guide](https://github.com/bitnami-labs/sealed-secrets)
  for key backup/recovery details.

## 3. Helm installation

The chart installs the controller, RBAC, CRD, and optional metrics Service.
RunnerPools and credential Secrets are separate resources.

Create `arc-values.yaml`:

```yaml
replicaCount: 1
imagePullSecrets:
  - name: forgejo-registry
resources:
  requests: {cpu: 100m, memory: 128Mi}
  limits: {memory: 256Mi}
metrics:
  enabled: true
```

The image repository, version tag, and pull policy come from the selected chart.
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
`runs-on` on both jobs to `debian-large`. Branch pushes, pull requests targeting
`main`, and manual runs perform formatting, race-enabled tests, `go vet`, Helm
lint/rendering, and generated-file checks. **Only Git tag pushes** build and
publish the image, after those checks succeed. The image job installs Docker CLI,
sets up Buildx, logs in using `REGISTRY_TOKEN`, and publishes the Git tag, short
commit SHA, and `latest`. After the image push succeeds, it publishes an OCI
Helm chart at `oci://git.shailesh.live/shailesh/charts/forgejo-arc` with the exact
Git tag as its `version`, `appVersion`, and default `image.tag`. The packaged
`image.repository` is `git.shailesh.live/shailesh/forgejo-arc`.

Use version tags such as `v0.1.2`, `0.1.2`, or `v0.1.2-rc.1`; both Helm and Docker
must accept the tag, so slashes and `+build` metadata are not supported. The `v`
prefix is preserved: a `v0.1.2` Git tag produces image `:v0.1.2` and chart version
`v0.1.2`, selected with `helm ... --version v0.1.2` or Argo CD
`targetRevision: v0.1.2`. `latest` follows the most recently published image tag,
including prereleases. The release only publishes artifacts; it does not deploy
them. When upgrading from an older example, remove explicit `image.repository`
and `image.tag` overrides so the chart's matching image is used.
The current repository workflow uses the production `debian-large` pool for both
checks and releases. For local development, change both jobs to `arc-minikube`,
served by [`config/samples/minikube-build-pool.yaml`](config/samples/minikube-build-pool.yaml).

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

For encrypted Git-managed credentials, follow [section 2.4](#24-store-kubernetes-credentials-with-sealed-secrets)
instead of applying the plaintext Secret examples below. The generated Secrets
have the same names, types, and keys.

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
      targetRevision: 0.1.2
      helm:
        releaseName: forgejo-arc-controller
        skipCrds: false
        values: |
          replicaCount: 1
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

The chart source's `targetRevision: 0.1.2` selects both the chart release and its
default controller image `git.shailesh.live/shailesh/forgejo-arc:0.1.2`. No Helm
image parameters or values are needed. Change that chart `targetRevision` to
upgrade to another release and its matching image.

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
  `helm upgrade --install` with your values. The versioned chart supplies its
  matching image tag; there is no need to track `latest` or set the tag separately.
  If a previous release saved an explicit image override, remove it from your
  values file and use `--reset-values --values arc-values.yaml` with the upgrade,
  including all other custom settings you want to retain in that values file.
- **Argo CD:** update the chart source's `targetRevision` in Git, then sync. The
  example inherits the image defaults from that chart. Remove any old image
  overrides from Helm parameters, inline values, and external values files;
  explicit overrides take precedence over the new chart's defaults.
- **Pools:** edit and apply/sync `resources`, `dockerResources`, `maxRunners`, or
  placement settings. Existing workers keep their snapshots. Scaling the
  controller Deployment does not change worker concurrency.

### Rotate credentials

If using Sealed Secrets, reseal and sync the encrypted source as described in
section 2.4 rather than editing its generated Secret directly. For directly
managed Secrets, update `forgejo-live-credentials` in place (repeat the token
Secret command).
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
| Downloads hang during TLS inside workflow containers | Compare pod and nested network MTUs. Controller builds including commit `57140cc` match DinD's default and workflow-created bridge MTUs to the pod interface; upgrade older controllers on VXLAN networks. |
| Build OOM or slow | Size `dockerResources`, including nested services/BuildKit; CPU limits throttle; inspect pod/container termination status |
| Argo CD cannot fetch chart | Repository Secret in `argocd`, `type: helm`, `enableOCI: 'true'`, correct username/PAT, URL without `oci://` |
| Argo CD cannot create pools | Chart CRD included, cluster RBAC allowed by AppProject and Argo CD credentials, destination Secrets present |
| Deletion is stuck | Keep controller and API credentials available; restore Forgejo connectivity and let running jobs finish before removing finalizers/dependencies |

The actual bootstrap procedure for this repository is also recorded in
[`docs/minikube-bootstrap.md`](docs/minikube-bootstrap.md). Argo CD reference:
[Helm OCI sources](https://argo-cd.readthedocs.io/en/stable/user-guide/helm/),
[private repositories](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/).
