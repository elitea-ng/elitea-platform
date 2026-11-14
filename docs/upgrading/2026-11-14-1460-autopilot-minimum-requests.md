# Autopilot-minimum resource requests, clearer GCS startup error (#1460)

Chart defaults for the containers below changed so that none requests less than
the GKE Autopilot minimum (50m CPU, 52Mi memory). Autopilot raised those
requests behind the chart's back, and Argo CD showed the workloads as
permanently out of sync.

- **Who is affected:** every install. Operators on GKE Autopilot with Argo CD
  see the drift disappear. Other clusters schedule slightly larger requests.
- **What changed:**
  - `nats.promExporter.merge.resources` and the new `nats.reloader.merge.resources`:
    50m / 64Mi requests, 128Mi memory limit (the upstream chart sets none).
  - `worker.materialInit.resources` (new): 50m / 64Mi, 128Mi limit; the init
    container had no resources.
  - memory request 32Mi -> 64Mi for `web`, `pgbouncer`, `objectInit` and
    `kedaCleanup`; the `kedaCleanup` template fallback CPU 10m -> 50m.
  - `values-gke.yaml` budget: steady state about +100m / +192Mi; the header
    table and the guide's figures are updated.
- **What to do:** nothing. If you set your own `resources` for any of these,
  keep at least 50m / 52Mi. `deploy/helm/tests/render-gke-budget.sh` now fails
  a render whose containers (init containers and hook Jobs included) fall below
  the floor.
- **elitea-main startup:** when `STORAGE_BACKEND=gcs` has no endpoint and no key
  file and the object-store readiness probe fails, the error now names the
  Workload Identity annotation and `roles/iam.workloadIdentityUser` binding.
  No setting changes.
- No migration.
