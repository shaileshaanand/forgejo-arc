# Design and compatibility

## Target

Tested against Forgejo 16.0.5, Forgejo Runner 13.0.0, and Minikube Kubernetes
1.34.5. Docker mode uses Docker 28.5.2 DinD. Native sidecars are required for Docker
mode; the Helm chart requires Kubernetes 1.33+. Server and runner versions are
independent.

The relevant v16 APIs are the scoped `actions/runners` registration/list/delete
endpoints and `actions/runners/jobs`. The latter returns waiting and running jobs
with `handle` identifying an attempt. It is not paginated in v16. Runner listing
is paginated; cleanup reads through all pages, including server-capped page sizes.
An omitted `labels` parameter differs from `labels=`. Lifecycle reconciliation
uses the unfiltered snapshot and performs label subset matching locally.

## Reconciliation

1. Read credentials and the pool's persisted Kubernetes Jobs.
2. Fetch an authoritative Forgejo queue snapshot; abort on API failure.
3. Sweep owned registrations whose worker record is absent.
4. Finish cleanup, process timeouts/cancellation, and count admitted workers.
5. For matching waiting attempts, inspect claims across all namespaces.
6. Create a suspended Job carrying the pool configuration snapshot and attempt
   identity. This is the durable claim and provisioning record.
7. Register an ephemeral runner. Persist the returned token/UUID in an owned
   Secret, then unsuspend the Job. A lost registration response is recovered by
   listing/deleting registrations matching the worker name and pool UID.
8. The stock runner declares its labels and requests exactly the attempt handle
   using `one-job --handle ... --wait`.
9. On termination, delete any remaining external registration and its Secret.
   Retain failed worker records while the attempt remains active to bound retries.

The controller is deliberately single-threaded at reconciliation level. One
leader per installation, live API reads for capacity/claims, and deterministic
worker names make provisioning idempotent across controller restarts. Multiple
pods may run jobs concurrently up to each pool's configured maximum. Overlapping
pools share a persisted claim: the first admitted pool owns that attempt through
its retry lifecycle. All pools must use the same canonical URL for a Forgejo
instance. Deploy one controller installation per cluster.

## Failure semantics

- API errors never mean an empty queue. Workers are preserved while reconciliation
  retries with controller-runtime backoff.
- Kubernetes Job retries are disabled. A failed startup can get a new worker only
  while Forgejo reports the attempt as waiting and its retry budget permits it.
- A running attempt is not replayed. If a node or runner disappears abruptly,
  Forgejo determines the task's terminal state using its own timeout settings.
- Cancellation/disappearance gets a 30-second grace period for final log upload.
  Workers are suspended and pods terminated before credentials are revoked.
- Pool deletion drains work already admitted. Worker and pool finalizers clean
  external registrations while credentials remain available.
- A completed Kubernetes Job indicates runner termination; workflow success or
  failure must be read from Forgejo. Tests check that distinction explicitly.

## Execution

Host mode executes in the runner container with a disposable `/data` volume.
Docker mode uses a privileged native sidecar with disposable Docker storage and
pod-local Docker API access. The runner container is non-root; it receives only
its own ephemeral credentials. Worker service-account token automounting is off.
Workflow containers in Docker mode reach services over Docker networks and
Forgejo over cluster networking.

The initial implementation uses polling and has no warm runner pool, webhook
listener, shared build cache, or cross-cluster scheduling. Operator state is held
in Kubernetes resources; no additional database is required.
