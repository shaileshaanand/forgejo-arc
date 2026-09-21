# Validation record

Validated on 2026-09-21 using the dedicated `forgejo-arc` Minikube profile.

## Versions

- Forgejo: **16.0.5**
- Forgejo Runner: **13.0.0**
- Kubernetes: **1.34.5**
- Minikube: **1.38.1**, Docker driver, 4 CPUs and 6 GiB memory
- Worker Docker-in-Docker: **28.5.2**

## Checks passed

- `go test -race ./...`: 17 tests covering API contracts, label matching,
  pagination, attempt ownership, concurrency, retries, registration recovery,
  configuration snapshots, and lifecycle behavior.
- `go vet ./...` and controller binary/image builds.
- `helm lint` and chart rendering.
- `make e2e`: the complete sequence passed against a real Forgejo server.

| Minikube scenario | Result |
| --- | --- |
| Idle pool status, immutable scope, deletion with invalid credentials | Passed |
| Automatically provisioned host-mode workflow | Passed |
| Five matrix jobs with a two-worker limit and return to zero | Passed |
| Controller restart and Forgejo API outage preserving the original worker | Passed |
| Invalid runner image followed by bounded startup recovery | Passed |
| Workflow cancellation and cleanup | Passed |
| Intentional workflow failure without automatic replay | Passed |
| Deletion of a running pod without registering a replacement runner for the in-flight attempt | Passed |
| Container job, service container, Docker action, default Node image | Passed |
| Organization pool serving two repositories | Passed |
| User-scoped and instance-scoped runners | Passed |
| Pool deletion draining a running workflow successfully | Passed |
| Helm install, upgrade to two replicas, execution, metrics, uninstall | Passed |

Tests inspect Forgejo workflow results rather than treating Kubernetes Job
completion as workflow success. Cleanup assertions check Kubernetes worker Jobs,
runner credential Secrets, and Forgejo runner registrations.

After validation, the development controller and Forgejo server remain running.
Both example pools are ready with zero active workers and zero waiting jobs.
The temporary Helm test release was uninstalled. Test state and credentials are
in gitignored `.local/`; the end-to-end script stopped its temporary port-forward.

Run the suite again with `make e2e`. Installation instructions and individual
test commands are in the [README](../README.md).
