---
title: "One chart: NATS bundled — BREAKING for Kubernetes installs"
issue: 1390
date: 2026-10-22
order: 20
---

**Affects:** every Kubernetes install that ran the `nats` and
`nats-bootstrap` charts (or the three Argo CD Applications `elitea-nats`,
`elitea-nats-bootstrap` and `elitea`). Compose is not affected.

**What changed.**

- The upstream NATS chart (1.3.9, unchanged) is a dependency of the `elitea`
  chart with `condition: nats.enabled`, **on by default**, pinned in the
  committed `Chart.lock`; `charts/` itself is never committed. How each
  install path gets it:
  - **The published OCI chart** (`oci://ghcr.io/elitea-ng/charts/elitea`,
    recommended) carries it: nothing to build.
  - **An Argo CD git source** (`path: deploy/helm/elitea`): Argo CD runs
    `helm dependency build` itself. A restricted AppProject must list
    `https://nats-io.github.io/k8s/helm/charts/` in `sourceRepos`, and the
    repo-server must reach it (since S10 also
    `https://qdrant.github.io/qdrant-helm`; see "DeepWiki, Inventory and the
    runtime on by default" above).
  - **The Helm CLI from a clone:** run `deploy/helm/elitea/build-deps.sh`
    (`helm dependency build` from `Chart.lock`) before `helm install`,
    `template` or `lint`.
  - **Air-gapped:** mirror the published OCI chart, which already carries
    the NATS chart, rather than building from a clone.
- The NATS CA, server and route certificates, the NetworkPolicy, the alerts
  and the approver policies (the old `nats` wrapper chart) render from
  `templates/nats/`; the stream and KV bootstrap Job (the old
  `nats-bootstrap` chart) from `templates/natsBootstrap/`. Resource names are
  unchanged: StatefulSet and Service `elitea-nats`, Issuer `elitea-nats-ca`,
  Job `elitea-nats-bootstrap`, JetStream PVC `elitea-nats-js-elitea-nats-0`.
- The `nats` and `nats-bootstrap` charts are no longer published, and
  `deploy/argocd/applications/` holds one Application, `elitea`.
- Values moved:

  | Was | Now |
  | --- | --- |
  | `elitea` chart `nats.service`, `.namespace`, `.port`, `.tls`, `.networkPolicyClientNamesManaged` | `natsClient.*` (same keys) |
  | `nats` chart `security.*`, `networkPolicy.*`, `alerts.*` | `nats.security.*`, `nats.networkPolicy.*`, `nats.alerts.*` |
  | `nats` chart `nats.*` (the upstream chart's values) | `nats.*` (same keys) |
  | `nats-bootstrap` chart values | `natsBootstrap.*` (same keys; `natsUrl` now defaults from `natsClient`) |
  | `deploy/helm/nats/values-ha.yaml` | `deploy/helm/elitea/values-nats-ha.yaml` |

  The chart refuses the old client keys under `nats` and names the new place.
- The NATS PodMonitor is off by default (`nats.promExporter.podMonitor.enabled`);
  the exporter sidecar still runs.
- **Ordering.** With Argo CD the server, its certificates and the bootstrap
  Job are in sync wave `-10` and the Job is a `Sync` hook, so the streams
  exist before the workloads (wave 0) start. With the Helm CLI the Job is a
  `post-install,post-upgrade` hook; the workloads restart until it has run.
  Do not use `helm install --wait` on a first install: it waits for workloads
  that wait for the hook that runs after the wait.
- **Your own NATS.** Set `nats.enabled=false` and point `natsClient` at your
  server (`natsClient.service`, `.namespace`, `.tls.issuerRef`). The
  bootstrap Job still runs against it; turn it off with
  `natsBootstrap.enabled=false` if you create the streams yourself.

**Data impact.** None when you follow the steps: the StatefulSet keeps its
name and volume claim template, so the new StatefulSet rebinds the existing
JetStream PVC (`elitea-nats-js-elitea-nats-0`, and `-1`/`-2` in HA). NATS is
down from deleting the old StatefulSet until the new pod is Ready (a minute
or two); the services reconnect on their own and model calls during the gap
fail their budget check. If the PVC is lost, the effect is the one described
in "NATS is secured" below: budget counters and rate-limit windows restart
from zero for the period, undrained write-behind deltas are lost, and
presence rebuilds within one heartbeat.

**What to do (Argo CD).** The old root prunes removed children, and each
child carries the `resources-finalizer.argocd.argoproj.io` finalizer, so
removing them from git as they are would delete every NATS resource. Detach
them first:

1. Turn off auto-sync on the root Application, so it does not prune before
   step 3:
   `kubectl -n argocd patch application <root> --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'`
2. Detach the two NATS Applications from their resources:
   ```bash
   for app in elitea-nats-bootstrap elitea-nats; do
     kubectl -n argocd patch application "$app" --type json \
       -p '[{"op":"remove","path":"/metadata/finalizers"}]'
     kubectl -n argocd delete application "$app"   # no finalizer: nothing cascades
   done
   ```
   (`argocd app delete <app> --cascade=false` does the same.)
3. Delete the old StatefulSet. Its selector names the old release
   (`app.kubernetes.io/instance: elitea-nats`), and a selector cannot be
   changed in place. Deleting it stops the NATS pod; the PVC stays.
   ```bash
   kubectl -n elitea delete statefulset elitea-nats
   kubectl -n elitea get pvc elitea-nats-js-elitea-nats-0   # still Bound
   ```
4. Update your values (the table above), and your copy of the root's
   applications directory: delete the `elitea-nats` and
   `elitea-nats-bootstrap` manifests, drop any `nats.service` parameter from
   the `elitea` Application, and point it at the new chart version.
5. Re-enable auto-sync on the root, and sync `elitea`. It adopts the
   Services, Certificates, Issuers and NetworkPolicy of the same names,
   creates the new StatefulSet (which rebinds the PVC), and runs the
   bootstrap in wave `-10`.
6. Verify: `kubectl -n elitea rollout status statefulset/elitea-nats`, the
   `elitea-nats-bootstrap` Job succeeded, and `elitea-scheduler`'s
   `/readyz/budget-writeback` is 200.

**What to do (Helm CLI).** Helm does not move objects between releases, so
uninstall the two old releases first; `helm uninstall` keeps the JetStream
PVC (it belongs to the StatefulSet's claim template, not the release).

```bash
helm -n elitea uninstall elitea-nats-bootstrap
helm -n elitea uninstall elitea-nats
kubectl -n elitea get pvc elitea-nats-js-elitea-nats-0   # still there
helm -n elitea upgrade elitea oci://ghcr.io/elitea-ng/charts/elitea \
  --version <version> -f your-values.yaml        # no --wait
```

cert-manager keeps an issued Secret when its Certificate is deleted (unless
it runs with `--enable-certificate-owner-ref`), so the new Certificates find
the same CA and server Secrets; where one is gone it is issued again, and
every client re-reads its certificate on the next handshake.
