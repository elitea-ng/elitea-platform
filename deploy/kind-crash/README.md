# Kubernetes crash-rehearsal profile (kind)

T1+ profile of the crash-recovery suite (`handoffs/crash-recovery-suite/DESIGN.md` §1.5, §3.6). It carries the
`KX-*` scenarios. Prepared, not run: no cluster was created, no image or chart was pulled, and nothing was
rendered against a live API. The design named minikube; the owner chose kind (v0.33.0, node image
`kindest/node:v1.37.0@sha256:a1ed56cf...80ae5`, already pulled).

## Files

| File | Purpose |
|------|---------|
| `kind-config.yaml` | Cluster `elitea-crash`, 2 nodes, pinned node image, kubelet `podPidsLimit: 128` on both |
| `values-crash.yaml` | `deploy/helm/elitea` values, layered after `values-standalone.yaml` |
| `values-nats-crash.yaml` | `deploy/helm/nats` overrides after `values-scale1.yaml` (PVC, no exporter) |
| `manifests/postgres.yaml` | PostgreSQL StatefulSet on a PVC, with the agent-state database |
| `manifests/rustfs.yaml` | Object store copied from `deploy/kind/manifests/infra.yaml`, pinned to the platform node |
| `crash-stack.sh` | `preflight`, `up`, `pin`, `faults`, `down` |
| `faults/` | `kx.sh`, `deny-worker-nats.yaml`, and the scenario table |

## Rules

- Mutual exclusion with the compose crash stack (`elitea-crash` project, 16 GB Docker VM). `preflight` and `up`
  refuse while that project has running containers. Never run both.
- Every command names `--name elitea-crash` / `--context kind-elitea-crash`. Other clusters are never touched.
- Network gate. `up` stops with exit 3 and a message before any pull unless `ELITEA_CRASH_K8S_PULLS_APPROVED=1`.
  It is resumable.
- Node roles. Control-plane = `elitea.ai/crash-role=platform` (Main x2, gateway, NATS, PostgreSQL, object store, edge).
  Worker node = `elitea.ai/crash-role=execution` (Worker, Supervisor, sandbox pods). `up` labels the nodes and removes
  the control-plane taint. A second variant that moves NATS to the execution node means editing the selector in
  `values-nats-crash.yaml`.

## Approvals needed

1. cert-manager chart (`charts.jetstack.io`) and `quay.io/jetstack` images.
2. NATS chart dependency (`helm dependency build`, not vendored here) and its sidecars (config-reloader; nats-box if
   enabled). The Prometheus exporter is off.
3. A registry the kind nodes can reach for the Supervisor `@sha256` reference (chart refuses anything else,
   `templates/sandbox/supervisor.yaml:4`). Set `ELITEA_CRASH_SUPERVISOR_IMAGE`.
4. Calico images, only if `kx.sh np-probe` shows kindnet does not enforce NetworkPolicy (KX-06). Config for it is
   commented in `kind-config.yaml`.
5. Any third-party image in `THIRD_PARTY_IMAGES` not in the local Docker store.
6. Three local builds (Main, Worker rust, gateway) from a branch containing #1160, with the binary-marker check; max
   2 concurrent Docker builds. Destructive scenarios (KX-04, KX-08) need per-batch consent.

## Expectations: D1, D2, G-WORKER-01

See `services/elitea-worker-rust/docs/recovery-guarantees.md`.

- D1. Stock Helm leaves `agentModelCheckpointRecovery` false, so a Worker loss is a typed failure (F). This profile sets
  it true (`values.yaml:3710`); the expected class for model-call cells becomes R·U.
- G-WORKER-01. The brief says main has no `agentNodeRecovery` chart key. This worktree has it
  (`values.yaml:3714`, `templates/worker/configmap-runtime.yaml:65`), and `values-crash.yaml` sets it true. If the
  tree under test lacks the key, Code recovery on Kubernetes is EXPECTED-GAP F until fix F5 adds it; set the key
  false or drop the line.
- D2. The Worker spool is an `emptyDir`. A pod delete (KX-01) loses unacknowledged frames; a container-only restart
  keeps them. Both variants are listed in `faults/README.md`.
- Code consumers also need Main's owner recovery (`main.runtime.codeOwnerRecovery`), which `values-crash.yaml` enables.
  Its `mainWorkloadIdentity` is a `CRASH_FILL_` placeholder.

## Secrets

The render references these Secrets (collected from a `helm template` of the profile). `crash-stack.sh` mints the
first three; `up` stops before the chart if the rest are missing:

- Minted: `elitea-main-db`, `elitea-main-storage-secrets`, `elitea-main-llm-gateway-secrets`.
- Operator-provided: `elitea-main-auth-material`, `elitea-runtime-material`, `elitea-worker-material`,
  `sandbox-material`, `elitea-llm-gateway-secrets`. Generators: `deploy/runtime/install-material.sh`,
  `deploy/scripts/gen-runtime-certs.sh`, `gen-sandbox-certs.sh`, `gen-gateway-certs.sh`.
- Issued by cert-manager from the chart (`elitea-*-nats-client-tls`, `elitea-main-gateway-client-tls`,
  `elitea-llm-gateway-server-tls`, `elitea-platform-edge-tls`).
- Not handled here: a mock LLM for the gateway (`deploy/mock-llm`), the Supervisor profile JSON in `sandbox-material`,
  and `worker.runtime.sandboxRuntimes` (empty). Code scenarios need all three.

## How crashctl targets this

A future `scripts/crash-recovery/lib/stack_kube.py` (not written) selects it with `crashctl up --profile kind` and
refuses while the compose project is up.

- Faults: the same commands as `faults/kx.sh`, run with `--context kind-elitea-crash`; node faults use `docker`.
- Collectors: `kubectl exec` into `postgres-0` for the read-only invariant queries, and
  `kubectl port-forward svc/elitea-nats 8222` for `/jsz` and `/healthz`.
- Teardown: `crash-stack.sh down`.

## Verified chart keys (`deploy/helm/elitea/values.yaml`)

`image.tag` 65; `postgresql.existingSecret/key` 82-83; `nats.service` 256; `dbInit.enabled` 326 and
`dbInit.externallyManaged` 339 (guard `templates/guards.yaml:110`); `objectInit` 408; `agentStateMigrations` 442;
`networkPolicies.main.noExternalIngress` 506-; `main.replicaCount` 564; `main.autoscaling.enabled` 2014-2015;
`main.podDisruptionBudget` 2021; `main.nodeSelector` 2035; `web.enabled` 2039; `scheduler.enabled` 2169;
`llmGateway.nodeSelector` 2408; `llmGateway.env.GATEWAY_SELF_LLM_ORIGINS` 2614; `llmGateway.egressPosture` 2828;
`deepwiki.enabled` 3001; `inventory.enabled` 3232; `worker.enabled` 3385, `worker.implementation` 3461, `worker.replicaCount` 3505;
`worker.runtime.agentModelCheckpointRecovery` 3710; `worker.runtime.agentNodeRecovery` 3714;
`worker.runtime.sandboxRuntimes` 3719; `worker.nodeSelector` 3800; `otelCollector.enabled` ~3806;
`sandboxKubernetes.enabled/executionNamespace/maxPods` 3894-3898; `sandboxKubernetes.supervisor.enabled/image/materialSecret/profiles`
3912-3916. `main.runtime.codeOwnerRecovery.*` and `main.runtime.sandboxAudiences` are not in `values.yaml`; they are read by
`templates/_code-nodes.tpl:17-125`. `helm template` of `values-standalone.yaml` + `values-crash.yaml` succeeded and the
output was checked for: Main replicas 2, node selectors on Main, gateway, Worker, session Job and cron, Worker
runtime.json with both recovery flags.

## Unverified

- NATS chart: dependencies are not vendored, so `values-nats-crash.yaml` was never rendered. The key
  `nats.podTemplate.merge.spec.nodeSelector`, the pod name `elitea-nats-0`, the JetStream PVC name, and the pod label
  `app.kubernetes.io/name=nats` are from upstream chart knowledge.
- No chart key places the platform edge (`elitea-platform-edge`), the Supervisor (`elitea-sandbox-supervisor`) or sandbox
  pods (G-CHART-PIN). `crash-stack.sh pin` patches the first two after install; a `helm upgrade` reverts it. Sandbox pod
  placement is decided by the Supervisor config in `sandbox-material`, not the chart; with one execution node and
  the Supervisor pinned there, sandbox pods land wherever the Supervisor's config allows (unchecked).
- `kind-config.yaml`: kubelet patch form `KubeletConfiguration.podPidsLimit` is the documented kind form; not run. Node
  labels and the taint are applied by `kubectl` after creation instead of kubeadm patches.
- kindnet NetworkPolicy enforcement on `v1.37.0`.
- `main.runtime.codeOwnerRecovery.mainWorkloadIdentity` value and the Supervisor HTTPS origin
  (`https://elitea-sandbox-deno:9446`) are guesses that pass the chart validator only.
- Default kind StorageClass `standard` (local-path) for the PVCs.
- `postgres.yaml` ordering: the agent-state database is created by initdb scripts; the chart's `agentState` checkpoint
  connection material lives in `elitea-worker-material` (operator-provided).
- The third-party image list was taken from the render plus manifests; `docker.io/library/nats:2.12.0` comes from
  `values-scale1.yaml`, the config-reloader image is not listed.
