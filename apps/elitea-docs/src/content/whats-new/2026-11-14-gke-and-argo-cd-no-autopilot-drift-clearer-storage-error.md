---
date: 2026-11-14
order: 9
---
## GKE and Argo CD: no Autopilot drift, clearer storage error

Every container the Helm chart renders now requests at least 50m CPU and 52Mi memory, the GKE Autopilot minimum, so Argo CD no longer shows the NATS sidecars, the worker's material init container, the web pod and the cleanup Job as permanently out of sync. When elitea-main cannot reach a GCS bucket with Workload Identity, the startup error now names the service-account annotation and the `roles/iam.workloadIdentityUser` binding to check. The GKE guide adds a wrapper-chart and hand-written-Ingress section and four more troubleshooting rows.
