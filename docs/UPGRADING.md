# Upgrading

Changes that need an operator to act when a deployment moves to a newer
release. Each entry says what changed, who is affected, and what to do. The
newest entry is first.

## LLM gateway: the degraded-mode budget cap is divided by the replica count

**Affects:** every install that runs more than one LLM gateway replica, and
every install that sets `LLM_BUDGET_EXPECTED_REPLICAS` or
`LLM_BUDGET_NATS_DEGRADED_CAP_USD`, and every client of the realtime route.

**What changed.**

* While NATS is down, each gateway replica now admits only its share of the
  degraded-mode budget cap: `floor(cap / LLM_BUDGET_EXPECTED_REPLICAS)`. The
  cap is `LLM_BUDGET_NATS_DEGRADED_CAP_USD`, else 10% of the hard limit.
  Before, each replica admitted the whole cap, so N replicas could spend
  N × cap.
* **Each request admitted while NATS is down reserves against the share**
  (`LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD`, default `0.10`, never more
  than the share) until its real cost is billed. Before, every request
  admitted ahead of the first bill passed, however many there were. Now one
  share admits at most share ÷ reservation requests at a time; the rest get
  402 until a request finishes. The fleet total is at most the cap, plus
  what a request costs above its reservation.
* **The per-replica cap shrinks as the replica count grows.** The chart sets
  `LLM_BUDGET_EXPECTED_REPLICAS` to the replica ceiling
  (`llmGateway.autoscaling.maxReplicas`, 4 by default, else `replicaCount`).
  With the defaults each replica gets a quarter of the cap. A cap smaller
  than the replica count, in nano-USD, gives a share of 0: that replica
  refuses every request for the budget until NATS returns.
* A replica that has not seen NATS healthy since it started gets no share:
  it refuses budgeted requests with 503 until NATS returns.
* A replica exactly at its share now refuses (before, it admitted one more
  request). In the near-limit state the remaining budget is divided too, so
  a remaining budget below the replica count, in nano-USD, refuses with 402.
* **Realtime sessions:** a live session keeps its reservation until it ends.
  When other spend fills the share, the session's next budget check returns
  402 and the session closes, as for any exhausted budget. A session's own
  reservation never refuses it.
* The gateway refuses to start when `LLM_BUDGET_EXPECTED_REPLICAS` is set to
  anything but digits for a number of 1 or more (`0`, `-1`, `+3`, `007` and
  text are refused). Before, such a value was read as 1.

**What to do.**

1. Leave `llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS` empty. The chart
   derives it. Outside the chart, set it to the most gateway replicas that
   can run.
2. To keep the degraded-mode allowance each replica had before, multiply
   `llmGateway.env.LLM_BUDGET_NATS_DEGRADED_CAP_USD` by the replica ceiling.
   This also raises the fleet total. A value of `0` (10% of the hard limit)
   is now the fleet total, not the total per replica.
3. Set `llmGateway.env.LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD` at or above
   the cost of your largest single request. A larger value admits fewer
   requests at once during an outage; a smaller one lets an expensive
   request pass its reservation.

## Scaling for every component: Inventory on N replicas, fleet-wide DeepWiki slots, autoscalers on by default

**Affects:** every Kubernetes install of the `elitea` chart, and especially
installs that set `postgresql.maxConnections` near the old budget, run
DeepWiki or Inventory without a database, read the DeepWiki slots, scrape
with a namespace label other than `namespace`, or bundle KEDA
(`keda.enabled`).

**What changed.**

* **Every autoscaler is on by default** (the Scaling page of the deployment
  docs has the table). New: elitea-web (CPU, 2 to 6), the LLM gateway (2 to
  4; `source: auto` falls back to CPU), Inventory (1 to 3), the OTel
  collector (CPU, 1 to 3), the egress proxy (CPU, 2 to 6, when enabled).
  `main.autoscaling.maxReplicas` 10 → 8. The worker no longer needs KEDA:
  `worker.autoscaling.source: auto` uses KEDA's `nats-jetstream` trigger when
  KEDA is there, else a CPU HPA (`keda` requires KEDA, `cpu` forces the HPA).
  Every KEDA ScaledObject also has a CPU trigger. A CPU HPA without
  metrics-server holds `minReplicas`. To keep a fixed count, set
  `<component>.autoscaling.enabled: false`.
* **Inventory runs on more than one replica.** The host keeps its
  invocations in the engine's database (`inventory_graph.invocations`,
  `invocation_events`, `host_replicas`, engine migration 0009) and receives
  `ELITEA_INVENTORY_DATABASE_URL` from the engine's secret, with its own pool
  `ELITEA_INVENTORY_DATABASE_MAX_CONNS` ("2"). The database role must be able
  to read and write those tables (the migration creates them as that role).
  The host gets the URL only while the migrate Job renders (the host runner
  `sidecar` and `inventory.migrate.enabled: true`), because only that Job
  creates its tables. Without it the host keeps its invocations in memory.
  More than one replica without a store is refused, for DeepWiki too; a
  fixture install without a database, or an Inventory install with
  `inventory.migrate.enabled: false`, sets `<app>.autoscaling.enabled: false`.
* **DeepWiki's generation slots are fleet-wide.** Migration 0009 adds
  `invocations.labels`, `invocations.holds_slot` and `host_replicas`. A
  generation is admitted only while the running generations are fewer than
  the slots (`ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS`, per replica) of all
  live replicas. A replica whose own slots are full still admits while the
  fleet has a free slot. When the whole fleet is full, the call ends at once
  with `service_busy` ("All N generation slots are busy"), and the poll body
  carries one more key, `busy_reason` (`fleet_full`, `draining` or
  `local_full`). Before, the slots were advisory and a second generation ran
  on the same replica anyway. Raise `ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS`
  or the replica ceiling if users start generations at the same time. A
  deletion and a cancel now stop runs on every replica, with or without
  NATS, and a deletion's stop fence (migration 0010, `host_stop_fences`)
  refuses a matching run that starts on any replica while the deletion runs.
* **Upgrade order.** The host checks `host_replicas` and `host_stop_fences`
  at start. Until the engine's migrate Job has applied migrations 0009 and
  0010, the host restarts; this clears itself when the Job runs (it is a
  pre-upgrade hook, so with Helm it runs first).
* **A mixed fleet during the rolling upgrade.** During a rolling upgrade, old
  and new replicas run at the same time. An old replica does not register in
  `host_replicas` and does not mark its generations as slot holders. The new
  replicas count each unfinished generation of an old replica as a used
  slot. The old replica adds no slots to the fleet capacity, so its
  generations only reduce the free slots. A deletion on a new replica stops
  the generations of old replicas by their tool, because their rows have no
  labels. Thus it stops all old-replica generations of that tool, not only
  those of the deleted project or wiki. An old replica does not see the stop
  until it ends or is reconciled, so the deletion can be refused until then.
  Do the deletion again after the upgrade. An old replica does not check the
  deletion fences, so it can start a new generation during a deletion. When
  an old replica stops, the new replicas end its unfinished rows after the
  owner grace, and its slots are free again. Old Inventory replicas keep
  their invocations in memory, so the new replicas do not see them at all;
  an ingestion that runs on an old replica is polled on that replica only
  until the rollout replaces it.
* **DeepWiki and Inventory drain for an hour.** `*_DRAIN_TIMEOUT_SECONDS`
  30 → 3600, `terminationGracePeriodSeconds` 60 → 3630, and a native preStop
  sleep of 5 s (`<app>.preStop.sleepSeconds`). On SIGTERM a replica stops
  admitting and finishes its running work. The engine is now a **native
  sidecar** (in `initContainers`, `restartPolicy: Always`): the kubelet stops
  it only after the host container has exited, so every engine call of a
  draining invocation finds it serving. Tools that read
  `spec.containers[1]` for the engine now read `spec.initContainers[0]`
  (`kubectl logs -c engine` does not change). A rollout of these Deployments can therefore take up to
  an hour while a build runs. The Cluster Autoscaler gives an evicted pod at
  most 600 s by default; see the Scaling page for
  `cluster-autoscaler.kubernetes.io/safe-to-evict`.
* **The connection budget fits the defaults with every autoscaler on.** The
  worker's AgentState pool is its own setting,
  `worker.runtime.limits.agentstate_max_connections` (default 8; was
  `delivery_max_concurrency`, 32), with
  `agentstate_acquire_timeout_millis` (30 000). The budget counts it, and the
  new Inventory host pool. The shipped defaults at every ceiling total 568
  against the default 600 (575 available). `values-standalone.yaml`
  280 → 420; `values-gke.yaml` stays at 500 (292 used).
* **The scheduler runs two replicas** (active/active, `RollingUpdate`), with
  a PDB and spread. It refuses more than two.
* **Balanced gRPC hops.** elitea-main dials DeepWiki and Inventory through
  new headless Services (`elitea-deepwiki-grpc`, `elitea-inventory-grpc`)
  with `round_robin`, and checks the certificate against the old Service
  name, so the certificates do not change. elitea-main dials elitea-vector
  the same way: the headless `elitea-vector-grpc`
  (`vector.service.grpcName`), with `ELITEA_VECTOR_GRPC_SERVER_NAME` set to
  `elitea-vector`. elitea-vector closes a client connection after
  `ELITEA_VECTOR_MAX_CONNECTION_AGE_SECONDS` (300), so the Rust clients (the
  worker, the engines), which dial the ClusterIP Service, also spread over
  its replicas. An explicit `ELITEA_<APP>_PLATFORM_GRPC_TARGET` or
  `ELITEA_VECTOR_GRPC_TARGET` still wins.
* **The Prometheus queries take the namespace label from
  `scaling.prometheus.namespaceLabel`** (default `namespace`; `""` drops the
  matcher) and add `scaling.prometheus.extraMatchers`. An annotation-based
  scrape that writes `kubernetes_namespace` no longer needs a hand-written
  `autoscaling.prometheus.query`. DeepWiki's and Inventory's queries now add
  the generations refused because the fleet was full
  (`elitea_subapp_slot_refusals_total{reason="fleet_full"}`, a new counter on
  the host's admin port, labelled `reason`; the postgresql source counts the
  rows with `busy_reason` `fleet_full`).
* **KEDA uninstall hook.** With `keda.enabled`, a pre-delete Job
  (`<release>-keda-cleanup`, the elitea-main image) deletes the release's
  ScaledObjects and TriggerAuthentications and waits for KEDA to run their
  finalizers before Helm removes KEDA (`kedaCleanup.timeoutSeconds`, 120).
  The manual step before `helm uninstall` is no longer needed, except with
  `--no-hooks` or a delete that skips Helm hooks.
* **preStop on elitea-vector and the OTel collector** (sleep 5, grace 30).

**What to do.**

1. If you set `postgresql.maxConnections`, render once: the refusal names
   each component's share. Lower a ceiling or a pool, or raise the number to
   match your server.
2. If your Prometheus labels the namespace other than `namespace`, set
   `scaling.prometheus.namespaceLabel` (and the Prometheus Adapter rule's
   overrides, if you use the bundled adapter).
3. If you run DeepWiki or Inventory without a database, set
   `<app>.autoscaling.enabled: false`.
4. If a fixed count is a requirement for a component, set
   `<component>.autoscaling.enabled: false` and its `replicaCount`.

## Scale-out readiness: optional KEDA and prometheus-adapter, an all-component connection budget, drain and grace periods

**Affects:** every Kubernetes install of the `elitea` chart, and especially
installs that set `postgresql.maxConnections` close to elitea-main's share,
override a gateway `preStop.command`, a grace period or
`LLM_BUDGET_EXPECTED_REPLICAS`, run Inventory on more than one replica, or
restrict `sourceRepos` in an Argo CD AppProject for a git source.

**What changed.**

* **Two optional dependencies, both off:** `keda` (kedacore chart 2.21.0,
  `keda.enabled`) and `prometheus-adapter` (prometheus-community chart
  5.3.0, alias `prometheusAdapter`, `prometheusAdapter.enabled`). Both are
  cluster-wide, so a cluster runs ONE of each: enable them only in a cluster
  that has none (`helm install` refuses when it can see one this release does
  not own). With KEDA already in the cluster, set nothing, or
  `scaling.keda.clusterInstalled: true` where the chart cannot see it (Argo
  CD). A git source in an AppProject that restricts `sourceRepos` must now
  also list `https://kedacore.github.io/charts` and
  `https://prometheus-community.github.io/helm-charts`, even with both off;
  the published OCI chart carries them.
* **Autoscalers for DeepWiki and the gateway pick their source.**
  `deepwiki.autoscaling` and `llmGateway.autoscaling` take `source: auto`
  (default), which uses KEDA's Prometheus trigger, then KEDA's postgresql
  trigger (DeepWiki) or a custom metrics adapter (gateway), then CPU. The
  gateway HPA no longer assumes an adapter: without one it scales on CPU.
  `llmGateway.autoscaling.maxReplicas` 10 → 4. The worker's ScaledObject is
  accepted with the release's KEDA, the cluster's, or
  `scaling.keda.clusterInstalled`.
* **Inventory runs on one replica.** Its host keeps invocations in memory;
  `inventory.autoscaling.maxReplicas` above 1 is refused.
* **The connection budget counts every component** at its replica ceiling:
  elitea-main, the gateway (`GATEWAY_DATABASE_MAX_CONNS`, new default `"8"`),
  the scheduler (`ELITEA_DATABASE_MAX_CONNS`, new default `"4"`), the DeepWiki
  host and engine (`ELITEA_DEEPWIKI_DATABASE_MAX_CONNS` `"4"`,
  `ELITEA_DEEPWIKI_QUERY_POOL_SIZE` `"8"`), the Inventory engine
  (`ELITEA_INVENTORY_POOL_SIZE` `"8"`) and the worker's AgentState pool
  (`postgresql.agentStateSharesServer`, default true). The refusal lists each
  share. `postgresql.maxConnections` 500 → 600; `values-standalone.yaml`
  210 → 280; `values-gke.yaml` 200 → 500 (a 4 vCPU / 16 GB Cloud SQL
  instance's default). An unset primary pool is refused even with the
  runtime plane off.
* **Drain and grace.** elitea-main gets a preStop sleep of 10 s,
  `ELITEA_SHUTDOWN_TIMEOUT: "30s"` and `terminationGracePeriodSeconds: 50`;
  the gateway's preStop is the native sleep action (`preStop.sleepSeconds:
  10`; `preStop.command` is REMOVED and refused) with a grace of 190;
  DeepWiki and Inventory drain for `*_DRAIN_TIMEOUT_SECONDS` 30; the worker's
  grace is 45. A grace period shorter than what runs inside it is refused.
  The native sleep action needs **Kubernetes 1.30 or newer**.
* **elitea-main's SSE caps** are 256 streams per replica, 16 per principal
  and 128 per project (were 16/4/8).
* **`LLM_BUDGET_EXPECTED_REPLICAS` is derived** from the gateway's replica
  ceiling (`autoscaling.maxReplicas`, else `replicaCount`). An explicit
  value below it is refused.
* **Spread and disruption budgets.** Each multi-replica workload gets soft
  zone and node spread (`scaling.topologySpread.enabled`). PodDisruptionBudgets
  render only where the floor is above one replica, and the gateway's and the
  worker's are on by default. No Deployment carries `replicas:` while an
  autoscaler owns it.

**What to do.**

1. Set `postgresql.maxConnections` to your server's real `max_connections`
   (`SHOW max_connections;`). If the render is refused, read the per-component
   list in the message and lower a pool or a replica ceiling, or raise the
   server.
2. Delete `llmGateway.preStop.command` and `llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS`
   from your values (the latter unless you want it above the ceiling).
3. Check that the cluster runs Kubernetes 1.30 or newer. On an older one,
   set `main.preStop.sleepSeconds: 0` and `llmGateway.preStop.sleepSeconds:
   0` (no preStop hook; the drain still runs after SIGTERM).
4. With Argo CD and a git source, add the two repositories above to the
   AppProject's `sourceRepos`.
5. To uninstall a release with `keda.enabled: true`, delete its
   ScaledObjects and TriggerAuthentications first, or the KEDA CRDs stay in
   `Terminating`.

The deployment docs' Scaling page has what each component scales on, the
budget formula and the per-replica caps.

## DeepWiki, Inventory and the runtime on by default; one switch per feature; Qdrant is the upstream chart — BREAKING

**Affects:** every Kubernetes install of the `elitea` chart. Installs that
relied on a feature being OFF by default, installs that ran the chart's own
Qdrant (`vector.qdrant.mode: internal`), and AppProjects that restrict
`sourceRepos` for a git source.

**What changed.**

* **On by default:** the runtime plane (`main.runtime.enabled`, with agent
  execution, index ingest and toolkit discovery on its default streams), the
  agent worker (`worker.enabled`), elitea-vector (`vector.enabled`) with a
  bundled one-node Qdrant (`qdrant.enabled`), DeepWiki (`deepwiki.enabled`)
  and Inventory (`inventory.enabled`). Other default changes:
  `postgresql.maxConnections` 100 → 500 (elitea-main's default ceiling of
  10 replicas at about 44 connections each; set it to your server's
  `max_connections`, and lower `main.autoscaling.maxReplicas` to fit, as
  `values-gke.yaml` does with 3), `vector.collection.replicationFactor` 2 → 1,
  `main.runtime.signingKeyId` `standalone-runtime-v1` (the default key id of
  `deploy/scripts/gen-runtime-certs.sh`). New, with the old behaviour as
  the default: `main.rollingUpdate` and `worker.rollingUpdate` (`maxSurge:
  1, maxUnavailable: 0`); `values-gke.yaml` sets both to `maxSurge: 0,
  maxUnavailable: 1`, so no rollout exceeds its 4 vCPU / 10 GiB budget.
* **One switch per feature.** The chart derives elitea-main's side from the
  provider's block: `ELITEA_DEEPWIKI_ENABLED` / `ELITEA_INVENTORY_ENABLED`, the
  git allowlists (copied from `deepwiki.env` / `inventory.env`), the client
  certificate paths and their mounts, the platform gRPC targets and the
  `ELITEA_VECTOR_*` settings. An explicit `main.env` value that agrees is kept;
  one that disagrees fails the render and names the switch to set instead.
  `main.fileConfig.deepwikiClientMaterial.enabled` and
  `inventoryClientMaterial.enabled` now default to `null` (follow the
  provider); an explicit `false` beside an enabled provider is refused.
  `main.runtime.agentExecutionDispatch` / `indexIngestDispatch` settings with
  `enabled: false` are no longer refused: `enabled` is the switch.
* **No hand-minted certificates.** With cert-manager, the DeepWiki and
  Inventory callback client certificates (`elitea-deepwiki-callback-client`,
  `elitea-inventory-callback-client`, one DNS SAN `elitea-subapp-callback`)
  and Qdrant's TLS certificate (`elitea-qdrant-tls`) are issued by the chart
  from the runtime-CA issuer (`elitea-runtime-ca`, kind `ClusterIssuer` by
  default; the same issuer as `vector.mtls.issuerRef`). To bring your own
  callback certificate, set `<app>.callbackClient.certManager.enabled: false`.
* **Fixed Secret names.** Every Secret the chart reads has a default name, and
  `externalSecrets.enabled` renders an ExternalSecret for each:

  | Secret | Keys |
  |---|---|
  | `elitea-main-db` | `database-url` |
  | `elitea-runtime-material` | the nine runtime files (`runtime-ca.crt`, `command-signing-key.pem`, `command-signing-keyring.json`, the control/output/content keypairs) |
  | `elitea-worker-material` | `runtime-ca.crt`, `agent-worker-client.crt/.key`, `command-signing-keyring.json`, `worker-output-spool-key`, `agent-checkpoint-connection` |
  | `elitea-main-auth-material` | `auth-attempt-key`, `auth-pat-signing-key`, `auth-form-users.json` |
  | `elitea-main-llm-gateway-secrets` | `gateway-identity-secret` |
  | `elitea-llm-gateway-secrets` | `identity-secret`, `secrets-master-key` |
  | `elitea-qdrant-api` | `api-key` (was `vector.qdrant.apiKeySecret.name`, no default) |
  | `elitea-deepwiki-secrets` | `identity-secret` |
  | `elitea-inventory-secrets` | `identity-secret` |

* **Missing values fail the render with ONE message.** A values file that
  lacks something only you know (the authentication document and its
  `public_origin`, `platform.aiProjectId`, `DEPLOYMENT_URL`,
  `worker.runtimeSession.expectedIdentity`, the two git allowlists,
  `networkPolicies.main.ingressFrom`, `GATEWAY_SELF_LLM_ORIGINS`,
  `llmGateway.egressPosture`, and `vector.qdrant.external.url` when Qdrant is
  not bundled) is refused with every missing value and every disagreeing
  switch listed together, starting `this install cannot render yet.`
* **Qdrant is the upstream qdrant chart** (1.19.2, `condition:
  qdrant.enabled`, pinned in `Chart.lock`, carried by the published chart)
  instead of the chart's own StatefulSet. It runs one node; layer
  `values-qdrant-ha.yaml` for three (Raft clustering with peer TLS, a
  PodDisruptionBudget, anti-affinity, `replicationFactor: 2`). REST and gRPC
  are TLS (the old StatefulSet was plaintext inside the cluster), peer traffic
  too when clustered. `qdrant.enabled: false` is bring-your-own Qdrant at
  `vector.qdrant.external.url` (`vector.qdrant.external.caSecretName` for a
  private CA). Two things the upstream chart cannot do itself, and how this
  chart covers them:
  - Qdrant runs as the chart's own ServiceAccount `elitea-vectordb`, with
    `automountServiceAccountToken: false`, because the subchart's account has
    no such setting.
  - The subchart cannot read `image.pullSecrets`: if Qdrant's image comes
    through your registry, list the same Secrets under
    `qdrant.imagePullSecrets` (the install NOTES warn when you set one and
    not the other).

  The upstream chart's `helm test` Pod is not supported. It is a
  `helm.sh/hook: test` resource, so `helm install` and `helm upgrade` never
  create it and Argo CD skips Helm test hooks; do not run `helm test` on
  this release.

  | Before | After |
  |---|---|
  | `vector.qdrant.mode: internal` | `qdrant.enabled: true` (the default) |
  | `vector.qdrant.mode: external` | `qdrant.enabled: false` with `vector.qdrant.external.url` |
  | `vector.qdrant.internal.replicas` | `qdrant.replicaCount` (above 1 needs clustering: use `values-qdrant-ha.yaml`) |
  | `vector.qdrant.internal.storage.size` / `.storageClassName` | `qdrant.persistence.size` / `.storageClassName` |
  | `vector.qdrant.internal.resources` | `qdrant.resources` |
  | `vector.qdrant.internal.image` | `qdrant.image` (`tag` without `-unprivileged`; `useUnprivilegedImage: true` adds it) |
  | `vector.qdrant.internal.podSecurityContext` / `securityContext` | `qdrant.podSecurityContext` / `qdrant.containerSecurityContext` |
  | `vector.qdrant.internal.nodeSelector` / `tolerations` / `affinity` | `qdrant.nodeSelector` / `tolerations` / `affinity` |
  | StatefulSet `elitea-qdrant` | StatefulSet `elitea-vectordb` |
  | PVCs `storage-elitea-qdrant-<n>` | PVCs `qdrant-storage-elitea-vectordb-<n>` |
  | Services `elitea-qdrant`, `elitea-qdrant-headless` | Services `elitea-vectordb`, `elitea-vectordb-headless` |
  | NetworkPolicy `elitea-qdrant-netpol` | NetworkPolicy `elitea-vectordb-netpol` |

  The render refuses `vector.qdrant.mode` and `vector.qdrant.internal` with
  this map, and refuses `qdrant.apiKey` (the upstream chart's `lookup` breaks
  `helm template` and Argo CD): the key always comes from
  `elitea-qdrant-api`.

**What to do.**

1. **Keep a feature off by saying so.** Each of these is now on unless your
   values set it to `false`:

   ```yaml
   main:
     runtime:
       enabled: false
   worker:
     enabled: false
   vector:
     enabled: false
   qdrant:
     enabled: false     # with vector off: the render refuses a Qdrant nobody reads
   deepwiki:
     enabled: false
   inventory:
     enabled: false
   ```

   The render refuses a switch that another feature still needs (vector off
   under DeepWiki, say) and lists every such conflict in one message.
2. **Give Qdrant a volume.** The bundled Qdrant claims a 50Gi
   PersistentVolumeClaim from the default StorageClass
   (`qdrant.persistence.storageClassName: ""`). A cluster with no default
   StorageClass needs one named.
3. **Create the Secrets under their fixed names** (table above), or let
   `externalSecrets` render them. Drop `vector.qdrant.apiKeySecret.name` if
   it named `elitea-qdrant-api`; another name still works when the
   `QDRANT__SERVICE__API_KEY` entry in `qdrant.env` names it too.
4. **Remove the derived settings from `main.env`** (`ELITEA_DEEPWIKI_*` /
   `ELITEA_INVENTORY_*` enable flags, allowlists and client paths) and the
   `main.fileConfig.*ClientMaterial` blocks, or keep them only where they
   agree. Delete a hand-made `elitea-deepwiki-callback-client` Secret, or set
   `callbackClient.certManager.enabled: false` to keep it.
5. **Move the Qdrant data.** The upgrade deletes the old `elitea-qdrant`
   StatefulSet; its PVCs stay, but the new `elitea-vectordb` starts empty.
   Either:
   * **snapshot and restore**: BEFORE the upgrade, for each collection,
     `POST /collections/<name>/snapshots` on the old Qdrant (port 6333, header
     `api-key`) and download the snapshot; AFTER it, upload each to the new one
     with `POST /collections/<name>/snapshots/upload?priority=snapshot` over
     HTTPS (trust the `ca.crt` of the `elitea-qdrant-tls` Secret); or
   * **rebuild**: regenerate the DeepWiki wikis, rebuild the Inventory graphs
     and re-run the index ingests; the sources of truth are in PostgreSQL and
     the git hosts.

   Then delete the old claims: `kubectl -n elitea delete pvc -l
   app.kubernetes.io/name=elitea-qdrant` (only the old `storage-elitea-qdrant-*`
   claims carry that label; the new ones are labelled `app: elitea-qdrant`).
   Check the list with `get` first.
6. **Argo CD git source:** a restricted AppProject must also list
   `https://qdrant.github.io/qdrant-helm` in `sourceRepos`. The published OCI
   chart carries the qdrant chart, so an OCI source needs nothing; an
   air-gapped install mirrors the OCI chart.

## GKE Ingress support and SSE keepalives on /llm and MCP streams

**Affects:** Kubernetes installs on GKE that publish elitea-main through a
standard Ingress; every client of `/llm` streaming and of MCP `tools/call`
streams (no action needed).

**What changed.**

* `main.ingress.gke` renders the GKE companions of the Ingress: the
  `cloud.google.com/neg` and `cloud.google.com/backend-config` annotations on
  the elitea-main Service (the `http` port only), a BackendConfig
  (`timeoutSec: 3600`, connection draining, HTTP `/healthz` health check on
  8080, optional `securityPolicy`) and a FrontendConfig (`redirectToHttps`,
  optional `sslPolicy`), bound with `networking.gke.io/v1beta1.FrontendConfig`.
  It turns on with `gke.enabled`, `className: gce` / `gce-internal`, or the
  same value in the `kubernetes.io/ingress.class` annotation.
* `main.ingress.certManager` (`issuer` or `clusterIssuer`, plus `issuerKind`
  and `issuerGroup` for an external issuer) annotates the Ingress for
  cert-manager. `main.service.annotations` is new.
* elitea-main writes `: keepalive` SSE comments into an idle `/llm` event
  stream (between events only) and into an MCP `tools/call` stream while it
  waits for the result, every 25 seconds of silence. Conforming SSE parsers
  ignore comments.

**What to do.**

* On GKE, set `main.ingress.gke.enabled: true` and admit `35.191.0.0/16` and
  `130.211.0.0/22` in `networkPolicies.main.ingressFrom`. If you already
  manage a BackendConfig or FrontendConfig, set its `create: false` and
  `name`.

## Helm 4 is the supported CLI; Helm 3 still renders the chart

**Affects:** anyone who runs `helm install` / `helm upgrade` by hand. Argo CD
installs are not affected.

**What changed.** CI lints, installs (the kind smoke) and publishes the charts
with Helm **4.3.1**, and checks on every pull request that Helm **3.22**
renders the `elitea` chart to the same objects, because Argo CD renders with
the Helm 3 it bundles. Supported: Helm 4.3 or newer for the CLI (recommended),
Helm 3.8 or newer where Argo CD (or another Helm 3 tool) renders the chart.

**What to do with the Helm 4 CLI.**

- `helm upgrade` of a release that Helm 3 installed keeps client-side apply
  (`--server-side=auto` follows the release's previous method); a new install
  uses server-side apply. Nothing to do unless an upgrade reports a
  field-manager conflict, then rerun it with `--force-conflicts`.
- `--atomic` is now `--rollback-on-failure`.
- `--wait` now waits with kstatus (`--wait=watcher`). The first-install rule
  is unchanged: no `--wait` on a FIRST install of the elitea chart (see "One
  chart: NATS bundled").
- `helm push`, `helm pull oci://…` and `helm registry login` are unchanged.

## One edge route: elitea-main serves `/` and relays `/app` and `/docs` to elitea-web — BREAKING

**Affects:** Kubernetes installs that publish the platform through
`main.ingress.enabled`, and any cluster that applied
`deploy/gateway-api/httproute.yaml` or a copy of it (#1359 P3).

**What changed.**

* **elitea-main is the browser edge in a cluster too.** The chart sets
  `ELITEA_WEB_UPSTREAM` to the elitea-web Service (`http://elitea-web:80`)
  whenever `web.enabled` is true. elitea-main then redirects `/` to `/app/`,
  relays `/app…` and `/docs…` to elitea-web, and keeps the paths it serves
  itself under `/app` — the MCP server at `/app/<project id>/mcp[/…]` and
  the two icon file servers — exactly as it does in compose.
* **The edge is ONE `/` rule to elitea-main**, on the chart's HTTPRoute, its
  Ingress, and `deploy/gateway-api/httproute.yaml`. The second HTTPRoute
  (`elitea-web`, `/app`, `/docs` and the `/` redirect) and the per-path rule
  list are gone. MCP clients now work on every Gateway: the carve-out once
  needed a RegularExpression path match, which the manifest never carried.
  The chart's HTTPRoute now renders `PathPrefix` for `main.ingress.pathType:
  Prefix` (it rendered `Prefix`, which Gateway API does not accept).
* **The plain-Ingress identity guard is gone**, with
  `main.ingress.identityHeadersStrippedByController`. elitea-main reads no
  `X-Auth-*` header and deletes every client identity header itself, so a
  plain Ingress with Form sign-in renders. The HTTPRoute keeps its
  `stripIdentityHeaders` filter as defence in depth.
* **Probes.** With the edge on, elitea-main refuses `/readyz` and `/startupz`
  to a non-loopback caller, so the startup and readiness probes run
  `/elitea-main -healthcheck <path>` inside the container. `-healthcheck`
  takes an optional path (`/readyz`, the default, `/startupz` or `/healthz`).
  The chart refuses any other `main.probes.{startup,readiness}.path` while
  the edge is on. Liveness stays an httpGet on `/healthz`. An older
  elitea-main ignores the path argument and reports `/healthz`, so a probe
  passes before the replica is ready: with `web.enabled` (the default),
  `main.image.tag` must be this release or later. Leave it unset to follow
  the chart's appVersion. `main.probes.execCommand` names the binary and
  `main.probes.*.timeoutSeconds` the probe timeouts.
* **NetworkPolicy.** elitea-web gets `elitea-web-netpol`: its port admits
  elitea-main, `networkPolicies.web.extraIngressFrom` and
  `networkPolicies.probeFrom` only.

**What to do.**

* A cluster that applied `deploy/gateway-api/httproute.yaml`: re-apply it, and
  delete the old `elitea-web` HTTPRoute (`kubectl delete httproute elitea-web`).
  Keep `elitea-main`'s HTTPRoute pointing at port 8080.
* An Ingress or route of your own that sent `/app` or `/docs` to elitea-web:
  send them to elitea-main instead, or simply route `/` to elitea-main.
* Remove `main.ingress.identityHeadersStrippedByController` from your values
  (it is ignored).
* Anything else that reached elitea-web directly (a debug pod, a scraper):
  list it in `networkPolicies.web.extraIngressFrom`.

## One chart: NATS bundled — BREAKING for Kubernetes installs

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

## pgvector is removed: every vector is in Qdrant behind elitea-vector — BREAKING

**Affects:** every install. PostgreSQL no longer needs (or gets) the `vector`
extension. DeepWiki and Inventory keep their vectors in elitea-vector (#1359;
ADR-0031 as amended, elitea-docs#58), so a deployment that runs either
engine's `native` runner, Rust indexing or the memory toolkit needs
`vector.enabled` (or an external elitea-vector).

**What changed.**

* **PostgreSQL is plain `postgres:18`** in every compose stack, kind and CI.
  The chart's `dbInit` job, the standalone `db-init` and kind's initdb no
  longer run `CREATE EXTENSION vector`. An existing data volume or server is
  reused as it is.
* **DeepWiki**: dense vectors live in elitea-vector (source `deepwiki`, the
  shared `emb_<slug>_<dim>` collections), one namespace per wiki; a build
  writes them under its own generation and the publish flips
  `wikis.active_generation` with the nodes and edges. The engine's new
  migration `0008_vectors_in_qdrant` drops `wiki_node_embeddings` (and its
  build twin) and adds `wikis.active_generation` and the
  `wiki_stale_generations` table.
  **Every wiki published before the upgrade has no dense vectors**: it
  answers from the lexical branch only until it is regenerated.
* **Inventory**: entity vectors live in elitea-vector (source `inventory`,
  their own float32 collections `inv_<slug>_<dim>`, exact search), one
  namespace per graph. The float8[] `entities.embedding` column stays. The
  engine's `0008_entity_vectors_in_qdrant` drops the generated
  `embedding_vector` column and adds the graph row's vector columns and
  `vector_staged_generations`. **No rebuild is needed**: the first ranking of
  a graph (which carries the invocation's bearer) publishes its stored
  vectors to elitea-vector (the backfill). `orphans --delete` now keeps a
  live project's graph that still holds vectors in elitea-vector; elitea-main's
  toolkit cleanup deletes both.
* **Rewritten released migrations.** Plain PostgreSQL could not run the
  engines' released chains, which created the extension, so these released
  files were rewritten without it:

  | Engine | Migration | Released checksum still accepted |
  |---|---|---|
  | DeepWiki | `0001_wiki_index_storage` | `ea5af872e89869cdf824e92f71586ed58143efa30572c74c73a3e7bfa1bb40c3` |
  | DeepWiki | `0003_build_space` | `e36c440fb1c6d43c2900931f94f4f1c1ea023359c9e9ffbddd93e191efaf2b7e` |
  | DeepWiki | `0005_project_scope` | `fe71faa1fc925a050ad73b5d3b7313e1cc8ca926d1218fe476ae950bd3a7cac7` |
  | Inventory | `0004_entity_vectors` | `a28e5e3845a21781ec21d3d376aadb8f66648b8cd731274caa6210cbc370b44f` |

  The runner (`libs/rust/pg-migrate`, `apply_with_rewrites`) accepts exactly
  the released checksum listed for each rewritten version, besides the new
  one; any other checksum is still refused, as for every other migration.
  A database that applied the released text keeps that checksum in its
  ledger and is not re-run; a fresh install records the new text. Each
  rewritten file names its released checksum in its header.
* **The extension is dropped only when nothing needs it.** Each engine's
  `0008` drops the `vector` extension with `RESTRICT` when it exists. When
  another object still depends on it, or the migrating role does not own
  it, the migration keeps it with a NOTICE and succeeds; drop it yourself
  once nothing uses it (`DROP EXTENSION vector;` as its owner).
* **The `pgvector` configuration type is removed** (and
  `pgvector_configuration` with it). Creating a configuration of type
  `pgvector`, or switching one to it, is refused with 400. **Existing rows
  are kept but inert**: they are hidden from configuration lists and
  selectors, nothing reads them, and `GET` and `DELETE` by id still work, so
  remove them (and their vault secrets) when convenient. Toolkit settings
  lose `pgvector_configuration`: the API strips it on read and write, and the
  tenant migration `0154_strip_toolkit_pgvector_configuration` deletes the
  key from stored `elitea_tools.settings`.
* **elitea-main's per-project PgVector store is gone**: no `project_<id>`
  database or role is created for a new project (`cmd/pgvector-orphans` is
  replaced by `elitea-main legacy-pgvector-orphans`, below).
  * With `ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED=false`, project create
    used to provision a `project_<id>` PgVector database. It no longer does,
    and **such a deployment indexes nothing** (it indexed nothing that was
    read since the Python worker went). Index through the Rust worker and
    elitea-vector.
  * The project delete response no longer carries `database`.
  * **A project delete still drops the project's legacy `project_<id>`
    database and `project_<id>_user` role**, as the cleanup-journal step
    `project_pgvector_drop`, through the new
    `ELITEA_LEGACY_PGVECTOR_ADMIN_URL` (the old store's admin connection; a
    secret). The step is on a delete's journal row when that URL is set, or
    when the delete found evidence of a legacy database: the project's
    `elitea-pgvector` configuration row, or the database or role on the
    platform database's own server. **Without the URL, such a delete keeps
    its journal row open** (`last_error` set, retried with backoff, logged at
    Error naming both variables) instead of completing over the data; set
    `ELITEA_LEGACY_PGVECTOR_RETIRED=true` instead when the server is gone
    with its databases, and those steps close without a drop. The two
    variables together are a boot error. A deployment that never ran the
    store sets neither and sees no change.
  * A journal row written before the upgrade decodes as before; one that
    still owes `project_pgvector_drop` (with `had_vector_store`) runs it under
    the rule above, and one that finished it does not run it again.
* **Token introspection** (`TokenIntrospectionService`) is also served on
  elitea-main's platform listener (`:9446`) behind the same
  `ELITEA_VECTOR_INTROSPECTION_CLIENTS`, so a stack without the runtime plane
  can run elitea-vector for DeepWiki and Inventory. Callback tokens of both
  engines now carry a grant to their own source, and their default lifetime
  is 8 h (`ELITEA_{DEEPWIKI,INVENTORY}_CALLBACK_TOKEN_TTL_MINUTES`).
* **The introspection pool is 2 connections** per elitea-main replica (was 8;
  `ELITEA_RUNTIME_DB_VECTOR_INTROSPECTION_MAX_CONNS`). `values-standalone.yaml`
  declares `postgresql.maxConnections: 210` (was 200): four replicas at 44
  connections plus 25 reserved. Turning vector on no longer costs a replica.
* **New engine settings**: `ELITEA_DEEPWIKI_VECTOR_URL` and
  `ELITEA_INVENTORY_VECTOR_URL` (the elitea-vector origin,
  `https://elitea-vector:9470`), with optional `*_VECTOR_SERVER_NAME`. The
  `native` runners refuse to start without them; TLS reuses each engine's
  callback CA, certificate and key. The chart derives them from
  `vector.enabled` (a value set in the app's `env` wins) and **refuses to
  render a `native` runner with neither**.

**What to do.**

1. Turn on elitea-vector before (or with) the upgrade wherever DeepWiki's or
   Inventory's `native` runner runs. With this chart:

   ```yaml
   vector:
     enabled: true            # needs main.runtime.enabled
     qdrant:
       mode: internal         # a 3-node StatefulSet; or external + external.url
       apiKeySecret:
         name: elitea-qdrant-api-key   # create it first; key: api-key
     # recommended in production: every model either engine or the worker
     # embeds with, as "<model-slug>:<dimension>"
     allowedSpaces: []
   ```

   ```bash
   kubectl -n <ns> create secret generic elitea-qdrant-api-key \
     --from-literal=api-key="$(openssl rand -hex 32)"
   ```

   The certificates come from the `elitea-runtime-ca` ClusterIssuer the
   runtime plane already uses (`vector.mtls.issuerRef`). Keep
   `vector.collection.replicationFactor` (2) at most
   `vector.qdrant.internal.replicas` (3).
   **argocd-lab (technicaldomain)** needs exactly this block in its values,
   plus the Secret above in its namespace, before it syncs this release,
   whenever its DeepWiki or Inventory engine runs `native` (the chart's
   default runner), which the chart now refuses without vector. If it also sets `postgresql.maxConnections` from
   `values-standalone.yaml`'s old 200, raise it (and the server) to at least
   201, or keep elitea-main at three replicas.
2. **Regenerate every DeepWiki wiki** after the upgrade (each one answers
   lexically until then). Inventory needs nothing: graphs backfill on their
   first ranking.
3. Roll the engines over completely: a replica still on the previous release
   fails dense searches and publishes once the new `0008` has dropped the
   embedding tables.
4. **Drop the per-project PgVector databases.** If the deployment ever ran
   the per-project PgVector store (the legacy runtime, or elitea-main before this
   release), point elitea-main at that server's admin role, the connection
   the legacy runtime kept in the public project's `elitea-pgvector` configuration (a
   role that may drop the `project_<id>` databases and `project_<id>_user`
   roles it created; `postgresql+psycopg://` is accepted):

   ```bash
   kubectl -n <ns> create secret generic elitea-main-legacy-pgvector \
     --from-literal=admin-url='postgresql://<admin>:<password>@<pgvector-host>:5432/postgres'
   ```

   The chart reads it as `main.secrets.ELITEA_LEGACY_PGVECTOR_ADMIN_URL`
   (Secret `elitea-main-legacy-pgvector`, key `admin-url`, optional); with
   compose, add `ELITEA_LEGACY_PGVECTOR_ADMIN_URL` to the `elitea-main`
   service's environment. From then on every project delete drops its own legacy
   database and role. For projects deleted EARLIER (by the legacy runtime, or by a
   release whose journal skipped the drop), run the one-shot command in the
   same image, with the same environment (`ELITEA_LEGACY_PGVECTOR_ADMIN_URL`
   and `DATABASE_URL`):

   ```bash
   # dry run: prints the platform database, its project count and id range,
   # and every project_<id> database/role whose project no longer exists
   kubectl -n <ns> exec deploy/elitea-main -- /elitea-main legacy-pgvector-orphans
   # drop exactly the N orphans the dry run listed
   kubectl -n <ns> exec deploy/elitea-main -- \
     /elitea-main legacy-pgvector-orphans --drop --confirm-count N
   ```

   An id counts as an orphan only with no `centry.project` row and no open
   `centry.project_deletions` row, and is re-checked right before its drop.
   Check the header before `--drop`: a wrong `DATABASE_URL`, or a PgVector
   server shared with another platform, makes other deployments' databases
   look orphaned. `--drop` is refused when orphans exceed 20% of the
   server's `project_<id>` databases (`--max-orphan-fraction`), unless you
   add `--force-fraction`, and when a platform database has no projects.
   Once the server is decommissioned, replace the URL with
   `main.env.ELITEA_LEGACY_PGVECTOR_RETIRED: "true"` (compose: the same
   variable): projects created before this release keep their
   `elitea-pgvector` configuration row, and deleting one with neither
   setting keeps its journal row open. A deployment that never ran the
   store needs neither. Remove the `pgvector` configuration rows when
   convenient.
5. If an engine's `0008` logged that it kept the extension, drop it once
   nothing else uses it.
6. Compose: nothing beyond the image change for the default stacks;
   `docker-compose.standalone-full.yml` already runs elitea-vector and now
   points both engines at it. For the dev stack's `deepwiki` profile see the
   next entry.

## Dev compose: object storage, the platform listener and elitea-vector for the `deepwiki` profile

**Affects:** the local dev stack `deploy/docker-compose.yml` only. The chart
and the other compose files are unchanged.

**What changed.**

* elitea-main now gets `STORAGE_BACKEND=s3` with the stack's own rustfs store
  (bucket `elitea-artifacts`), as the e2e and standalone stacks do, and waits
  for `rustfs-bucket-init`. Without it, elitea-main refused to start on this
  stack: `STORAGE_BACKEND must be one of "s3", "azure", "gcs"`.
* The `deepwiki` profile gains `qdrant` and `elitea-vector` (the engine keeps
  its dense vectors there; see "pgvector is removed" above). elitea-vector's
  server and client certificates come off the provider CA, issued by
  `gen-platform-listener-certs.sh`, and it verifies the engine's tokens on
  elitea-main's platform listener. `postgres` stays plain `postgres:18`.
* The `deepwiki` profile can now reach elitea-main's platform gRPC listener
  (`:9446`, #1359). The KNOWN LIMIT about a missing listener is gone. The
  host uploads its artifacts over `ArtifactObjectService`, and the engine
  makes its model calls over `ModelService`. Both present the
  sub-application callback identity (`DNS:elitea-subapp-callback`). The
  engine's `native` runner used to refuse to start on this stack, because
  `ELITEA_DEEPWIKI_MODEL_SERVICE_URL` was unset. It now starts.
* A new unprofiled init service, `platform-material`, installs the listener's
  key into the named volume `platform_main`, owned by 65532 with mode 0600,
  because elitea-main refuses a key that a bind mount leaves 0644. It
  installs nothing and exits 0 while `ELITEA_RUNTIME_PLATFORM_ADDRESS` is
  unset, so the default stack needs no new certificates.
* This stack still has no LLM gateway. ModelService runs elitea-main's `/llm`
  surface, which exists only with `LLM_GATEWAY_URL`, so a model call fails
  there (`Unavailable`, HTTP 503). The refusal names the cause:
  `llm_gateway_not_configured`, "LLM_GATEWAY_URL is empty". Generating a wiki
  with a model needs `docker-compose.standalone-full.yml`.
* This stack still does not migrate the platform database (`SKIP_MIGRATIONS=1`;
  the schema comes from a loaded dump, `docker-compose.staging.yml`).

**What to do.** To run DeepWiki, run
`deploy/scripts/gen-platform-listener-certs.sh` after `gen-deepwiki-certs.sh`.
Then add `ELITEA_RUNTIME_PLATFORM_ADDRESS=:9446` to the `ELITEA_DEEPWIKI_*`
variables on the `--profile deepwiki up` command. The comment on the
elitea-main service and the root README give the full command. Nothing
changes for the default stack, apart from the storage fix above.

## DeepWiki and Inventory replicas share invocations; owner rows are marked

**Affects:** DeepWiki and Inventory installs with a database
(`<PREFIX>DATABASE_URL`), above all a rolling upgrade from an image before
#1359. No migration and no setting.

**What changed.**

* Each sub-app host process holds a PostgreSQL advisory lock for as long as
  it lives, on a dedicated connection, keyed by a 64-bit hash of its owner
  id. A sibling checks the locks of the owners with invocations in flight at
  startup and every minute after, and terminates an owner's invocations as
  orphans only when its lock is still free 30 seconds (three claim checks)
  after the sibling first saw it free, with no held sighting between: an
  owner whose session dropped re-takes its lock well within that, and keeps
  its runs.
* The host checks its own lock every 10 seconds. When its session dropped
  (a PostgreSQL restart, a failover, an idle-session kill) it refuses new
  invocations (gRPC `UNAVAILABLE`) and fails readiness until it has re-taken
  the lock: `owner_claim: false` in its gRPC health, and `503` from
  `/readyz` on its admin port, which the chart's readiness probe and the
  compose healthcheck read, so the replica leaves the Service. The host
  sends GOAWAY to every client connection after a minute, so elitea-main's
  connection moves to a ready replica. A run a sibling terminated in that
  gap keeps the terminated answer.
* The host has a second, plain-HTTP port for its probes only:
  `ELITEA_<APP>_ADMIN_ADDR` (default `:9090`; chart
  `deepwiki.admin.port` / `inventory.admin.port`) serves `/healthz` and
  `/readyz`. The chart's probes moved from `tcpSocket` on `platform-grpc`
  to `httpGet` on `admin`; the NetworkPolicy admits
  `networkPolicies.probeFrom` (if any) to it and nothing else. The chart
  renders the variable itself and refuses it in `<app>.env`.
* Rows written by this release carry an owner prefixed `claim1:`. Rows from
  an older image carry no prefix, and that image takes no lock, so a free
  lock means nothing for them. They keep the rule the older image applied
  itself: a process that **starts** terminates every such row in flight
  (its startup reconcile), and the periodic pass never touches them. So
  during a rolling deploy from an older image, a new replica starting ends
  what the old replicas are running at that moment — exactly what an
  old replica starting did before.

**What to do.**

* To keep in-flight DeepWiki/Inventory runs across the upgrade from an
  older image, let them finish before rolling; otherwise they end with "The service restarted while this
  invocation was running", as on any restart before.
* A run an old replica accepted after the last new replica started is not
  ended until some host process starts again. If one still reports in
  progress once the rollout is done, restart the hosts once
  (`kubectl rollout restart deployment/elitea-deepwiki` /
  `deployment/elitea-inventory`).
* A pooler in front of the database must keep a client's session
  (session pooling, not transaction pooling) for the lock to mean anything;
  point `<PREFIX>DATABASE_URL` at PostgreSQL or at a session-mode pool.

## A direct client is keyed by its socket address; compose trusts no proxy

**Affects:** every install with a Form authentication document
(`trusted_proxy_cidrs`), and compose stacks built from
`deploy/runtime/auth.form.yml` (#1359).

**What changed.**

* elitea-main is now the compose browser edge, with no proxy in front. The
  compose document used to trust all of RFC1918 (the compose network), so a
  browser's own `X-Forwarded-For` was believed and a client could name a
  fresh address per request to get a fresh login attempt window.
  `deploy/runtime/auth.form.yml` now trusts loopback only (`127.0.0.1/32`,
  `::1/128`), which in a compose stack means no proxy at all.
* A request from a peer outside `trusted_proxy_cidrs` is keyed by its socket
  address, and its `X-Forwarded-For` is ignored. Before, the Form login and
  logout routes answered 503 to such a peer.

**What to do.**

* Compose: nothing. Recreate the stack to pick up the new document.
* Kubernetes: `trusted_proxy_cidrs` (and `ELITEA_TRUSTED_PROXY_CIDRS`) must
  name only the ranges your Gateway or ingress controller forwards from. The
  example overlays (`values-auth-minimal.yaml`, `values-standalone.yaml`,
  `deploy/kind/values-kind.yaml`) name the whole RFC1918 space as a pod
  network placeholder; narrow it to your pod CIDR. Never include a range a
  browser reaches elitea-main from directly (a NodePort or LoadBalancer
  Service). The chart sets no default.

## Artifacts and internal MCP tools go over gRPC on :9446; the HTTP callback origins are gone

**Affects:** every install with `main.runtime.enabled`, every install that
runs the agent worker, DeepWiki or Inventory, compose stacks built from
`docker-compose.standalone-full.yml`, `docker-compose.yml` and
`docker-compose.e2e-standalone.yml`, and the kind stack (#1359 U5).

**What changed.**

* The last internal HTTP hops into elitea-main are gRPC on its platform
  listener (`:9446`), beside ModelService:
  * **`elitea.runtime.v1.ArtifactObjectService`**
    (`libs/proto/elitea/runtime/v1/artifact_object.proto`): the worker's
    artifact reads and writes (Office toolkits, indexing artifact sources),
    the DeepWiki/Inventory **hosts'** page and graph uploads, and the
    engines' artifact-folder reads. Each RPC runs the same artifact route a
    browser request takes. Refusals carry the `elitea-artifact-refusal-bin`
    trailer.
  * **`elitea.runtime.v1.InternalToolService`**
    (`libs/proto/elitea/runtime/v1/internal_tool.proto`): the worker's
    internal MCP toolkits (`ListTools`, `CallTool`, which replaced its MCP
    HTTP calls to `/app/{project}/mcp/{scope}`) and the Inventory engine's
    `investigate` source-tool calls (**`InvokeToolkit`**, which replaced its
    HTTP call to `POST /api/v2/elitea_core/test_tool/prompt_lib/{project}/{toolkit}`).
    Refusals carry the `elitea-internal-tool-refusal-bin` trailer. External
    MCP clients keep the HTTP MCP route on the public listener.
* **New elitea-main settings** `ELITEA_ARTIFACT_SERVICE_CLIENTS` and
  `ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS`: the workload identities
  (`dns:<name>` or `spiffe://…`, comma-separated) admitted to each service.
  **An empty list leaves the service unregistered**, so every caller is
  refused; elitea-main still starts.
* **Retired, refused at boot:** `ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL` (the
  worker's internal MCP origin), `ELITEA_DEEPWIKI_CALLBACK_BASE_URL` and
  `ELITEA_INVENTORY_CALLBACK_BASE_URL` (the providers' artifact origin), when
  set to any non-empty value; the chart refuses the keys even when empty. An
  empty Inventory callback base no longer means "source expansion off":
  source tools always go over InvokeToolkit. elitea-main hands the providers a fixed
  `llm_settings.api_base` of `https://elitea-platform.invalid/llm/v1`, which
  only names the route.
* **Internal MCP catalogue entries use the origin
  `https://internal-tools.elitea.invalid`** (`/app/{project}/mcp/{scope}`
  below it). The origin only identifies an entry as internal; the worker
  never dials it, and calls InternalToolService instead. Anything that builds
  or matches internal MCP entries by the old platform-listener or
  `elitea-platform-edge` origin must use this one.
* **The provider HOST containers now dial the platform listener too**, with
  `ELITEA_<APP>_ARTIFACT_SERVICE_URL` (`https://elitea-main:9446`) and the same
  `ELITEA_<APP>_CALLBACK_CA_FILE` / `_CERT_FILE` / `_KEY_FILE` as the engine,
  so **every** DeepWiki or Inventory install needs the runtime plane and the
  callback client certificate, whatever its engine runner (the fixture runner
  included).
* No provider pod reaches elitea-main on 8080 any more.

**What to do (Helm).**

1. Remove `main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL`,
   `main.env.ELITEA_INVENTORY_CALLBACK_BASE_URL`,
   `main.env.ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL`,
   `main.runtime.agentExecutionDispatch.currentMainBaseUrl` and
   `deepwiki.callbackViaPlatformListener` (any value). The chart refuses each
   one with a pointer here.
2. `deepwiki.enabled` and `inventory.enabled` now **always** need
   `main.runtime.enabled` and `<app>.callbackClient.secretName` (`tls.crt` /
   `tls.key` from `subapp-callback-client.crt` / `.key`; one Secret can serve
   both). The chart mounts it with the runtime CA into the host container as
   well, and sets `ELITEA_<APP>_ARTIFACT_SERVICE_URL`; `deepwiki.env` /
   `inventory.env` may not set it.
3. The chart renders both allowlists: the worker's
   `worker.runtimeSession.expectedIdentity` (when `worker.enabled`),
   `dns:elitea-subapp-callback` (artifacts: DeepWiki or Inventory enabled;
   internal tools: Inventory enabled), plus the new
   `main.runtime.artifactService.extraClients` and
   `main.runtime.internalToolService.extraClients`. A worker fleet outside
   this release goes in those lists (`values-standalone.yaml` names the
   default standalone worker identity there). Do not set the env names
   yourself: the chart refuses them.
4. The elitea-main NetworkPolicy no longer admits the DeepWiki or Inventory
   pods on 8080; they are admitted on 9446 only.

New render refusals: the five retired keys above; DeepWiki or Inventory
without the runtime plane or `callbackClient.secretName` (no longer only for
a native engine); `ELITEA_<APP>_ARTIFACT_SERVICE_URL` in `deepwiki.env` /
`inventory.env`; a hand-set `ELITEA_ARTIFACT_SERVICE_CLIENTS` /
`ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS`; a malformed or non-list
`extraClients`; `extraClients` with the runtime off.

**What to do (compose).** `docker-compose.standalone-full.yml` drops the
three retired variables, sets both allowlists on elitea-main
(`${RUNTIME_WORKER_SPIFFE_ID}` plus `dns:elitea-subapp-callback`), and gives
`elitea-deepwiki` / `elitea-inventory` the artifact URL and callback material
from a new volume, `standalone_runtime_subapp_host`, which `runtime-material`
fills for the hosts' uid (65532; the engines' copy is owned by 10001 with a
0600 key). `deploy/runtime/install-material.sh` is updated for it, so recreate
`runtime-material`. `standalone-stack.sh check` now also probes
ArtifactObjectService and InternalToolService with grpcurl.
`docker-compose.e2e-standalone.yml` has no runtime plane but now opens the
platform listener anyway: **elitea-main opens it whenever
`ELITEA_RUNTIME_PLATFORM_ADDRESS` is set, runtime on or off**, because its
services run public routes in process and need no runtime plane. Its
material comes from the new `deploy/scripts/gen-platform-listener-certs.sh`
(off the provider CA), which `apps/elitea-web/scripts/e2e-stack.sh up` runs.
`docker-compose.yml` opens no platform listener, so its providers have no
artifact transport: an invocation that carries a callback bearer fails its
upload, and the error names `ELITEA_<APP>_ARTIFACT_SERVICE_URL`.
(Since fixed: see "Dev compose: object storage, the platform listener and
elitea-vector for the `deepwiki` profile" above.)

**What to do (kind).** `deploy/kind/kind-stack.sh up` now installs the
in-repo NATS chart and its bootstrap, creates `elitea-runtime-material`,
`elitea-worker-material` (the runtime CA only) and
`elitea-deepwiki-callback-client` from `gen-runtime-certs.sh`, and
`values-kind.yaml` turns on the runtime plane's base. Recreate an existing
kind cluster (`down`, then `up`).

## Compose stacks no longer run Traefik; elitea-main is the browser edge

**Affects:** local and CI compose stacks only: `deploy/docker-compose.yml`,
`deploy/docker-compose.standalone-full.yml` and
`deploy/docker-compose.e2e-standalone.yml` (#1359). Kubernetes installs are
unaffected: the Gateway API HTTPRoute (`deploy/gateway-api/httproute.yaml`)
still routes `/app` and `/docs` to elitea-web and terminates TLS, and the chart
never sets the new variable.

**What changed.**

* The `traefik` service and `deploy/traefik/` are gone. elitea-main publishes
  its own `:8080` listener on the port Traefik used, so browser origins stay
  the same:

  | Stack | Browser origin | Override |
  |---|---|---|
  | `docker-compose.yml` | `http://localhost:8080` | (fixed) |
  | `docker-compose.standalone-full.yml` | `http://localhost:8084` | `STANDALONE_PORT` |
  | `docker-compose.e2e-standalone.yml` | `http://localhost:8082` | `E2E_PORT` |

* `ELITEA_WEB_UPSTREAM` (e.g. `http://elitea-web:80`) turns on elitea-main's
  compose edge (`internal/api/webedge`). It relays `/app…` and `/docs…` to
  that origin, except the paths elitea-main serves under `/app`
  (`/app/<n>/mcp[/…]`, `/app/application_icon/`, `/app/application_tool_icon/`),
  redirects `/` to `/app/` with a 302 that keeps the query string, and answers
  404 for `/readyz` and `/startupz` to anything but a loopback caller. The
  client identity headers are stripped by elitea-main itself, as before.
  Unset, nothing changes: elitea-main serves its API and nothing else.
* The relay is plain HTTP. It carries browser traffic, not a service call.

**What to do.**

* Recreate the stack (`podman compose ... up -d --remove-orphans`, or
  `deploy/scripts/standalone-stack.sh up`) so the old `traefik` container is
  removed. Nothing else changes for a browser, Playwright or the OIDC
  redirect URIs, because the published ports are the same.
* A custom compose file that used `deploy/traefik/dynamic*.yml` must set
  `ELITEA_WEB_UPSTREAM` on elitea-main and publish its `:8080` instead.
* Do not set `ELITEA_WEB_UPSTREAM` in a cluster. (Superseded by "One edge
  route" above: the chart now sets it itself.)

## Runtime content listener (:9445) and supervisor owner calls are gRPC

**Affects:** every install with the runtime plane enabled
(`main.runtime.enabled`, `ELITEA_RUNTIME_CONTENT_ADDRESS`): elitea-main, the
agent worker, and the sandbox supervisor; also installs that set
`ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG` (#1359).

**What changed.**

* elitea-main's content listener (`:9445`, same address, TLS files and client
  CA) now serves **`elitea.runtime.v1.RuntimeContentService` over gRPC** and
  no HTTP at all: agent inputs, runtime context and tokens, artifacts, Code
  workspace / debug / platform / recovery documents, toolkit discovery
  results, sandbox bundles and compiled snapshots. Inputs and bundle files
  stream in 64 KiB chunks after a header carrying length and SHA-256. The
  execution claim and fence travel as `elitea-claim-id` /
  `elitea-claim-fence-bin` call metadata (they replaced `X-Elitea-Claim-Id` /
  `X-Elitea-Fence`); a sandbox grant travels in the request (it replaced
  `X-Elitea-Sandbox-Bundle-Grant` / `-Compiled-Grant`). The JSON documents
  themselves are unchanged. The listener also answers `grpc.health.v1` to
  any runtime-CA workload identity, so a client certificate must carry
  exactly one DNS or SPIFFE SAN (the certificates `gen-runtime-certs.sh` and
  the chart issue already do).
* Main's owner calls to the sandbox supervisor (node Code recovery read /
  seal-no-effect, Code platform retained runtime / pending call / reply) are
  RPCs on `elitea.runtime.v1.SandboxSupervisorService`; the JSON-over-HTTP/2
  routes `/elitea.runtime.node-code-recovery.v1/...` and
  `/elitea.runtime.code-platform.v1/...` are gone.
* Health and metrics are unchanged: elitea-main's probes stay plain HTTP on
  `:8080`, the supervisor's readiness stays a TCP check, and neither needs a
  client certificate.

**What to do.**

1. **Roll elitea-main, the agent worker and the sandbox supervisor
   together.** An old worker or supervisor cannot read content from a new
   elitea-main, nor the other way round. With the chart, one `helm upgrade`
   does it.
2. Keep `content_origin` (worker) and the supervisor's content `origin` in
   their `https://host:port` form; the port is now required and is dialled
   as gRPC. `ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG.supervisors[].https_origin`
   keeps its name and form but must now include the port of the
   supervisor's gRPC listener (the one `SandboxSupervisorService` already
   used); an origin without a port is refused at startup.
3. Replace any HTTP probe or `curl` check against `:9445` with
   `/elitea-grpc-health --addr elitea-main:9445 ...` from the elitea-main
   image (the standalone stack's `check` does this). A plain TLS handshake
   check still works.

## Model calls from the worker and the engines go over ModelService (gRPC) on :9446

**Affects:** every install with `main.runtime.enabled` (the platform
listener), every install that runs the agent worker, DeepWiki or Inventory,
compose stacks built from `docker-compose.standalone-full.yml`, and anyone
running `cutover-ctl sse-flush-check` / `overhead-check` (#1359 U2).

**What changed.**

* The agent worker and the DeepWiki/Inventory engines no longer POST to an
  HTTP `/llm/v1` callback. They call **`elitea.llm.v1.ModelService`**
  (`libs/proto/elitea/llm/v1/model.proto`) on elitea-main's platform listener
  (`:9446`), which runs each call through the same `/llm` pipeline a browser
  request takes (bearer, project selection, budget and policy gates). A
  refusal comes back as a gRPC status plus the `elitea-model-refusal-bin`
  trailer carrying the original HTTP status and body.
* **New required elitea-main setting `ELITEA_MODEL_SERVICE_CLIENTS`**: the
  workload identities (`dns:<name>` or `spiffe://…`, comma-separated) allowed
  to call ModelService. elitea-main refuses to start without one while the
  platform listener is open. The worker is its certificate's SPIFFE ID; both
  engines are `dns:elitea-subapp-callback` (the
  `subapp-callback-client.crt` that `deploy/scripts/gen-runtime-certs.sh`
  mints).
* **New engine settings** `ELITEA_DEEPWIKI_MODEL_SERVICE_URL` /
  `ELITEA_INVENTORY_MODEL_SERVICE_URL` (`https://elitea-main:9446`) on the
  engine container, with `ELITEA_<APP>_CALLBACK_CA_FILE` (the runtime CA) and
  `ELITEA_<APP>_CALLBACK_CERT_FILE` / `_KEY_FILE` (the callback client
  certificate). `llm_settings.api_base` (from
  `ELITEA_<APP>_CALLBACK_BASE_URL`) now only names the route and stays the
  **artifact** hop's HTTP origin until #1359 U5. Without `MODEL_SERVICE_URL`
  the engine dials the api_base origin and refuses anything but https (or
  loopback http), so a `http://elitea-main:8080` callback base alone means no
  model calls.
* `deepwiki.callbackViaPlatformListener` now moves only DeepWiki's artifact
  hop. **Keep it off until #1359 U5**: the platform listener serves gRPC
  only, so with the flag on the host's artifact uploads and the engine's
  artifact reads (HTTP) fail there.
* `cutover-ctl sse-flush-check` and `overhead-check` now drive elitea-main's
  public `/llm` with a bearer (`--base-url`, default `$DEPLOYMENT_URL`;
  `--api-key` / `$API_KEY`; optional `--project-id` / `--projects-file`)
  instead of the gateway's old HTTP port; `budget-check --gateway-url` names
  the gateway's gRPC listener.

**What to do (Helm).**

1. **`worker.runtimeSession.expectedIdentity` is required whenever
   `worker.enabled`**: the worker certificate's SPIFFE ID (its one URI SAN;
   `gen-runtime-certs.sh` default
   `spiffe://elitea.standalone/ns/default/sa/agent-worker`). The chart puts it
   in `ELITEA_MODEL_SERVICE_CLIENTS`, and the runtime-session Job checks it
   against the certificate, so a wrong value fails the install. Read it with
   `openssl x509 -in agent-worker-client.crt -noout -ext subjectAltName`.
2. Callers outside this release (a worker fleet of its own, a callback
   certificate with another SAN) go in `main.runtime.modelService.extraClients`
   (list). Do not set `main.env.ELITEA_MODEL_SERVICE_CLIENTS`: the chart
   renders it and refuses a hand-set one.
3. DeepWiki and Inventory with the native engine now **need
   `main.runtime.enabled`** and a callback client Secret: create it from
   `subapp-callback-client.crt` / `.key` (`tls.crt` / `tls.key`) and name it
   in `deepwiki.callbackClient.secretName` (default
   `elitea-deepwiki-callback-client`) and the new
   **`inventory.callbackClient.secretName`** (default
   `elitea-inventory-callback-client`; DeepWiki's Secret can be reused). The
   chart mounts the runtime CA (only `runtime-ca.crt` of
   `worker.materialSecretName`) and the Secret into the engine container and
   sets the four engine variables; `deepwiki.env` / `inventory.env` may not
   set them. The fixture engine runner calls no model and needs neither.
4. Remove `inventory.engine.callbackCa` (refused): the model origin is always
   the platform listener, whose CA the chart mounts.
5. The NetworkPolicy on elitea-main now admits the DeepWiki and Inventory pods
   on 9446 whatever `callbackViaPlatformListener` says; 8080 still admits
   them for the artifact hop.
6. Do **not** add `https://elitea-main:9446` to
   `llmGateway.env.GATEWAY_SELF_LLM_ORIGINS`: that list is HTTP `/llm`
   origins a credential's `api_base` could name, and 9446 is gRPC-only behind
   a runtime-CA client certificate.

New render refusals: an empty `ELITEA_MODEL_SERVICE_CLIENTS` with the runtime
on; the worker without (or with a non-`spiffe://`) `expectedIdentity`; a
malformed `extraClients` entry; `extraClients` with the runtime off; a
hand-set `ELITEA_MODEL_SERVICE_CLIENTS`; a native DeepWiki/Inventory engine
without `main.runtime.enabled` or its `callbackClient.secretName`; the
engines' model-hop variables in `deepwiki.env` / `inventory.env`; and
`inventory.engine.callbackCa`.

**What to do (compose).** Re-run `deploy/scripts/gen-runtime-certs.sh` if
`deploy/certs/runtime/subapp-callback-client.*` is missing.
`docker-compose.standalone-full.yml` sets `ELITEA_MODEL_SERVICE_CLIENTS` on
elitea-main (`${RUNTIME_WORKER_SPIFFE_ID}` plus `dns:elitea-subapp-callback`;
export the same `RUNTIME_WORKER_SPIFFE_ID` you minted with), and
`runtime-material` installs the engines' CA, certificate and key into the new
`standalone_runtime_subapp` volume. `standalone-stack.sh check` now calls
ModelService with grpcurl and expects the pipeline's 401 for its invalid
bearer. The dev `docker-compose.yml` opens no platform listener, so its
`deepwiki` profile can run only the fixture engine runner. (Since fixed: see
"Dev compose: object storage, the platform listener and elitea-vector for
the `deepwiki` profile" above.)

## DeepWiki and Inventory: provider SPI and engine sidecar over gRPC; cancel fan-out over NATS

**Affects:** every install with `deepwiki.enabled` or `inventory.enabled`
(or `ELITEA_DEEPWIKI_ENABLED` / `ELITEA_INVENTORY_ENABLED` on elitea-main),
and compose stacks that run `elitea-deepwiki` / `elitea-inventory` (#1359).

**What changed.**

* elitea-main reaches each sub-application host over **gRPC on mutual TLS**,
  `elitea.subapp.v1.ProviderService` (describe, health, slots, invoke as a
  server stream, get-invocation, cancel), on the host's platform port
  (`9443`, `platform-grpc`). The host's HTTPS SPI listener (`8443`) and
  elitea-main's reverse proxy to it are gone. The browser-facing
  `/api/v2/deepwiki` and `/api/v2/inventory` routes are unchanged.
* The host and its engine sidecar speak **gRPC over the shared Unix socket**,
  `elitea.engine.v1.EngineService` (no TLS: the socket directory is the only
  way in). The NDJSON-over-HTTP protocol (`/engine/invoke`, `/engine/health`)
  is gone. **Roll the host image and the engine image together**: an old
  host cannot talk to a new engine, nor the other way round. Both run in one
  Pod, so one `helm upgrade` does it.
* A cancel that lands on one host replica now stops the run on whichever
  replica holds it: the hosts publish it on core NATS,
  `elitea.subapp.<app>.cancel.<invocation>`, in a new **`SUBAPP`** account
  (identities `elitea-deepwiki`, `elitea-inventory`; no JetStream).

**What to do.**

1. Remove `main.env.ELITEA_DEEPWIKI_BASE_URL` / `ELITEA_INVENTORY_BASE_URL`.
   The chart and elitea-main refuse them. The chart derives
   `ELITEA_<APP>_PLATFORM_GRPC_TARGET` (`elitea-<app>-svc:9443`); set it
   yourself (host:port, no scheme) only for a provider outside this release.
2. Remove `deepwiki.service.port` / `inventory.service.port` (refused): the
   Service exposes `platformGrpc.port` only. `platformGrpc.mtls` is
   mandatory and `platformGrpc.clients` must name elitea-main (the default).
3. Unset `ELITEA_<APP>_LISTEN_ADDR` on the host (refused at start); it
   listens on `ELITEA_<APP>_PLATFORM_GRPC_ADDR` (default `:9443`) and
   requires `ELITEA_<APP>_PLATFORM_CLIENTS`.
4. Upgrade the NATS chart (`deploy/helm/nats`) **before** the platform: it
   adds the `SUBAPP` account, and its guards refuse a values file that
   declares accounts without it. With NATS TLS on, the platform chart issues
   the `elitea-deepwiki` / `elitea-inventory` client certificates and mounts
   them. Without `ELITEA_<APP>_NATS_URL` the host starts and logs that the
   fan-out is off; that is fine for one replica only.
5. Compose: the hosts serve gRPC on 9443 only; elitea-main reads
   `ELITEA_<APP>_PLATFORM_GRPC_TARGET=elitea-<app>:9443`
   (`deploy/scripts/gen-nats-certs.sh` now also mints the two SUBAPP
   identities for the secured NATS overlay).

## The platform-edge Traefik is gone; elitea-main serves the worker over gRPC + mTLS on :9446

**Affects:** every install that runs the agent worker (`worker.enabled=true`),
sets `main.runtime.currentMainBaseUrl` to an https origin, or sets
`deepwiki.callbackViaPlatformEdge`. Compose: `docker-compose.standalone-full.yml`
(#1359).

**What changed.** The worker's `platform_origin` (its `/api/v2` + `/llm` calls,
the Office host, indexing artifact reads and internal MCP tools) used to go
through `elitea-platform-edge`, a second Traefik with its own certificate.
elitea-main now serves that surface itself on an internal **platform
listener**, `main.runtime.listeners.platformAddress` (default `:9446`), over
TLS 1.3 that **requires a client certificate signed by the runtime CA**. It
reuses the content listener's keypair (`content-server.crt`, SAN
`elitea-main`). The NetworkPolicy admits the worker (and, with the new flag
below, DeepWiki) to 9446.

**:9446 is a gRPC listener, not HTTP.** Every internal hop is gRPC over mTLS:
the port speaks HTTP/2 gRPC only (ALPN `h2`), and each gRPC service on it
admits only the workload identities (certificate SPIFFE URI or DNS SAN)
configured for it. It serves no REST route: `curl`, `openssl s_client` with an
HTTP request line, or an HTTP liveness probe against 9446 no longer gets an
answer. Check it with gRPC health (`grpc.health.v1.Health/Check`, open to any
runtime-CA client with a workload identity), for example with the
`elitea-grpc-health` tool in the elitea-main image:
`/elitea-grpc-health --addr elitea-main:9446 --ca runtime-ca.crt --cert
agent-worker-client.crt --key agent-worker-client.key`. The address,
certificate and chart/env names (`ELITEA_RUNTIME_PLATFORM_*`,
`main.runtime.listeners.platformAddress`) are unchanged.

**What to do.**

1. Delete `worker.platformEdge` from your values. The chart refuses to render
   while `worker.platformEdge.enabled` is true, or while
   `worker.runtime.platformOrigin` still names `elitea-platform-edge`.
2. Set `worker.runtime.platformOrigin: https://elitea-main:9446` (the new
   default) if you override it.
3. Delete the `elitea-platform-edge-tls` Secret; nothing reads it.
4. elitea-main no longer calls itself: the next-input-suggestion policy
   lookup is in process. `main-platform-client.crt` / `.key` and
   `ELITEA_RUNTIME_PLATFORM_CLIENT_CERT_FILE` / `_KEY_FILE` are not read any
   more; remove them from the runtime material Secret and any env overrides.
   `main.runtime.currentMainBaseUrl` stays (it is the internal MCP origin
   handed to the worker); `https://elitea-main:9446` needs no Main client
   certificate. `deploy/scripts/gen-runtime-certs.sh` deletes the retired
   files from an existing tree.
5. `deepwiki.callbackViaPlatformEdge` was removed and now fails the render; it
   is not mapped onto the new key because the new hop needs a client
   certificate you have to mint. Use `deepwiki.callbackViaPlatformListener:
   true` and create the `deepwiki.callbackClient.secretName` Secret
   (default `elitea-deepwiki-callback-client`, keys `tls.crt` / `tls.key`) from
   `subapp-callback-client.crt` / `.key` (runtime CA, clientAuth EKU).
6. If `GATEWAY_SELF_LLM_ORIGINS` lists the worker's model origin, replace
   `https://elitea-platform-edge/llm/v1` with `https://elitea-main:9446/llm/v1`.
7. Compose: re-run `deploy/scripts/gen-runtime-certs.sh` (it removes the
   retired `platform-edge.*` and `main-platform-client.*` files and issues the
   sub-application callback identity), then
   recreate the stack. The `standalone_runtime_edge` volume is unused and can
   be removed.
8. `main.env.LLM_GATEWAY_URL` must name the gateway's gRPC listener,
   `https://elitea-llm-gateway-grpc:8443` (`llmGateway.service.grpcName` /
   `grpcPort`). The chart refuses the HTTP port (`:8083`, health and metrics
   only), which values written for the edge Traefik often still name.

## The X-Auth-* edge identity plane is removed; Form sessions resolve in elitea-main — BREAKING for external auth edges

**Affects:** a deployment whose own edge (outside this repository) calls
`elitea-main`'s `/internal/auth/main` or `GET /auth` as a forward-auth target
(Traefik `forwardAuth`, a Gateway API `ExtensionRef`, NGINX `auth_request`),
or that sends `X-Auth-*` identity headers to `elitea-main` in any other way.
A deployment that uses only this repository's chart or compose files has
nothing to do.

**What changed.** (#1359)

- `GET /internal/auth/main`, `GET /auth` (the edge-auth token check) and the
  Form plane's `/auth/check` are gone and answer 404. No `elitea-main` route
  emits an `X-Auth-*` header any more, and none reads one.
- **Form sign-in sessions now resolve in `elitea-main` itself.** The
  `elitea_browser_auth` cookie that `/auth/form/authorize` sets is read
  in-process by every API route and the admin console, exactly like the single
  sign-on `elitea_session` cookie (including the cross-site write check). No
  edge ForwardAuth is needed for `ELITEA_FORM_LOGIN_ENABLED=true` — before
  this change, a Form sign-in authenticated nothing behind an edge that did not
  run one, which was every edge this repository ships.
- `elitea-main` deletes the inbound identity headers (`X-Auth-Type`,
  `X-Auth-ID`, `X-Auth-User-ID`, `X-Auth-Reference`, `X-Auth-Signature`,
  `X-Auth-Avatar`, `X-Auth-Avatar-State`, `X-Auth-Session-Id`,
  `X-Auth-Session-Name`, `X-Auth-Session-Endpoint`,
  `X-Elitea-Identity-Signature`, `X-Elitea-Project-Id`, `X-Elitea-User-Id`,
  `X-Elitea-Tenant-Id`) on every request before any route runs, so it no
  longer depends on an edge to strip them. `X-Elitea-Execution-Id` is kept: the
  desktop and the runtime worker send it on `/llm`, and `elitea-main` verifies
  it against a live execution of the caller before using it.
- The worker's private edge (`platform-edge`) no longer authenticates the
  execution-events stream: the worker's bearer token is read by
  `elitea-main` directly, as on every other route.

**What to do.** Remove any forward-auth middleware that points at
`/internal/auth/main` or `/auth`; with it in place every request is refused,
because the target answers 404. Route browser traffic straight to
`elitea-main` — it authenticates the Form cookie, the single sign-on cookie and
bearer tokens itself. Keep stripping the identity headers at the edge if you
like; it is now defence in depth rather than the only control. The auth
document is unchanged; its `mappers`, `authorization` and `credential_headers`
sections are still validated but no longer used.

## Default-deny NetworkPolicies; an ingress decision is required

**Affects:** every install of the `deploy/helm/elitea` chart. A CNI that
enforces NetworkPolicy (Calico, Cilium, etc.) is needed for the objects to take
effect.

**What changed.** The chart now renders ingress NetworkPolicies, on by default
(`networkPolicies.enabled: true`):

- `elitea-main`: port 8080 admits the platform edge, the DeepWiki and Inventory
  pods that call back directly, and the peers you list; the runtime listeners
  admit the worker pods only.
- the platform edge: port 443 from the worker, `elitea-main` and (when
  `deepwiki.callbackViaPlatformEdge` is set) DeepWiki; egress only to
  `elitea-main` and cluster DNS.
- the worker: no inbound connections. Worker egress is not restricted.
- the sandbox supervisor: its profile ports admit the worker and `elitea-main`.

**What to do.** The chart cannot see the pods that deliver outside traffic to
`elitea-main:8080`, so it refuses to render until you state who they are:

- list the Gateway API gateway or ingress controller as NetworkPolicyPeer
  entries in `networkPolicies.main.ingressFrom` (or `extraIngressFrom`). This
  includes a Gateway API HTTPRoute applied outside the chart, such as
  `deploy/gateway-api/httproute.yaml`; and `main.ingress.enabled=true` needs
  `ingressFrom` itself; or
- set `networkPolicies.main.noExternalIngress=true` when nothing outside the
  chart's own pods reaches `elitea-main` (for example access by
  `kubectl port-forward` only). It is refused together with
  `main.ingress.enabled=true`.

If your own policy already covers these workloads, set
`networkPolicies.enabled=false` together with
`networkPolicies.externallyManaged=true`. If your CNI applies policy to kubelet
probes, list the probe sources in `networkPolicies.probeFrom`. The Argo CD
sample has a new parameter for the gateway namespace
(`networkPolicies.main.ingressFrom[0]...`) next to the other values to fill in.

**Related, unchanged checks.** (Superseded by "One edge route" above: the
guard below is removed.) A plain `networking.k8s.io/v1` Ingress with Form
sign-in is still refused unless the controller removes the inbound `X-Auth-*`
and `X-Elitea-*` identity headers and
`main.ingress.identityHeadersStrippedByController=true` says so. (Superseded
for `elitea-main` itself by the entry above: it now strips those headers and
serves no forward-auth endpoint.)

## DeepWiki indexes are scoped by project; existing indexes are deleted — BREAKING

**Affects:** every deployment that runs the DeepWiki native engine
(`deepwiki.engine.runner: native`, or `ELITEA_DEEPWIKI_RUNNER=native`).

**Why.** The engine kept every wiki index in one `deepwiki` database keyed
by `wiki_id` alone, and a `wiki_id` is derived from the repository name
(`owner--repo--branch`, `artifact--bucket--prefix--branch`), not from the
project. Each index row now belongs to one project, and every access is
scoped by `(project_id, wiki_id)`. Wiki ids are no longer shared between
projects.

**What changed.**

- Engine migration `0005_project_scope.sql` recreates the index tables
  (`wikis`, `wiki_nodes`, `wiki_edges`, `wiki_node_embeddings`,
  `wiki_bm25_*`) with `project_id` leading every key, and adds
  `project_id` to `deepwiki_build.builds`.
- **Every existing index is deleted**, and every build in progress with it.
  A row written before 0005 cannot be attributed to a project (the same
  wiki id may have been generated by several projects), so it is not kept.
- The generated wiki pages are **not** touched: they live in the platform
  artifact store (the project's `wiki-artifacts` bucket), not in this
  database. The wiki browser lists and shows them as before.
- Until a wiki is generated again, `ask` and `deep_research` on it answer
  "No wiki index found for … Please generate a wiki first." (and the
  wiki_query `resolve_and_ask` / `resolve_and_deep_research` report the same
  failure in band).
- The Go sub-application host stamps the authenticated project on every
  engine call (`_elitea_project_id`), from the verified identity signature
  when `ELITEA_DEEPWIKI_IDENTITY_SECRET` is set on the host, else from the
  `llm_settings.organization` the platform facade writes. The engine
  refuses `generate_wiki`, `ask` and `deep_research` without it.

**What to do.**

1. Upgrade the engine image and the Go host together, and let the rollout
   finish before anyone regenerates a wiki. The migrate Job applies 0005
   before the new pods start. An engine pod from before 0005 cannot open or
   publish a build (it does not write the `NOT NULL` `project_id`), and it
   does not filter its reads by project. Finish the rollout (or scale the
   DeepWiki deployment down) before anyone regenerates a wiki.
2. Set `ELITEA_DEEPWIKI_IDENTITY_SECRET` on BOTH elitea-main and the
   DeepWiki host (`deepwiki.secrets.ELITEA_DEEPWIKI_IDENTITY_SECRET`) if you
   have not: without it the host has no verified identity and falls back to
   the facade-written `llm_settings.organization`, which is only as
   trustworthy as the mTLS hop to the host.
3. Regenerate the wikis whose chat (`ask` / `deep_research`) your users
   need. The pages of a wiki keep working without it.

## DeepWiki runs the native engine; the Python engine image is gone — BREAKING

**Affects:** every Helm install with `deepwiki.enabled: true`, every values
file or Argo CD Application that sets `deepwiki.engine.*` or
`deepwiki.env.ELITEA_DEEPWIKI_RUNNER`, and compose stacks started with
`deploy/docker-compose.deepwiki-native.yml`.

**What changed.**

- The DeepWiki engine sidecar is always the Rust-native engine,
  `ghcr.io/elitea-ng/elitea-deepwiki-engine-native` (ADR-0026). The chart no
  longer runs the Python `ghcr.io/elitea-ng/elitea-deepwiki:<tag>-engine`
  image, as the sidecar or as the migrate Job.
- The default changes from `legacy` to `native`:
  `deepwiki.engine.runner: native` and
  `deepwiki.env.ELITEA_DEEPWIKI_RUNNER: "native"`.
- `deepwiki.engine.runner` accepts `native` (the engine) and `fixture`
  (canned results, for a stack that proves the hop). Both run the native
  image; `fixture` reads no database.
- `legacy` is **refused**, not aliased, on both settings, with a message
  that names this entry. It named the Python engine, and a silent alias would
  hide that the engine, its image and its resource needs all changed.
- `deepwiki.engine.image` and `deepwiki.engine.resources` (the Python
  sidecar's) are gone, and the chart **refuses to render** a values file
  that still sets either. The native sidecar's keys are unchanged:
  `deepwiki.engine.native.image`, `.resources`, `.workerCpuSeconds`.
- The `native` runner needs an `ELITEA_DEEPWIKI_DATABASE_URL` secret:
  `deepwiki.secrets.ELITEA_DEEPWIKI_DATABASE_URL`, else
  `postgresql.existingSecret`. The chart refuses to render without one (a
  plain `deepwiki.env` value does not reach the migrate Job). The engine
  keeps every index in that database.
- The migrate Job always runs the native image's `migrate`. It writes the
  same `schema_migrations` ledger with the same checksums as the Python
  runner, so a database the Python runner migrated needs no extra step.
- Compose: `deploy/docker-compose.standalone-full.yml` runs the native
  sidecar and its `elitea-deepwiki-migrate` service itself, with the
  `fixture` runner by default (`DEEPWIKI_NATIVE_RUNNER=native` for the
  engine). `deploy/docker-compose.deepwiki-native.yml` is deleted. The dev
  `deploy/docker-compose.yml` `deepwiki` profile runs the Go host, the
  native sidecar and the native migration.
- Outside Helm too: the Go host (`elitea-subapp-host`) no longer has a
  `legacy` runner. `ELITEA_DEEPWIKI_RUNNER=legacy` stops it at start with
  a pointer to this entry, as the native engine binary already did.
- The Python service (`services/elitea-deepwiki`) is deleted, and
  `ghcr.io/elitea-ng/elitea-deepwiki` is no longer built or published.
  Tags already published stay in the registry; nothing in the chart pulls
  them.

**What to do.**

1. Remove `deepwiki.engine.image` and `deepwiki.engine.resources` from every
   values file. Move any image or resource override to
   `deepwiki.engine.native.image` / `deepwiki.engine.native.resources`
   (`limits.memory` in Gi or Mi, at least 1205Mi; it sets the generation
   worker's address-space cap).
2. Replace `runner: legacy` and `ELITEA_DEEPWIKI_RUNNER: "legacy"` with
   `native`, or delete them to take the default.
3. Make sure a database URL secret reaches the component (step above). Its
   role needs `CREATE` on the database for migration 0003 (see
   `services/elitea-deepwiki-engine/README.md`).
4. Mirror `ghcr.io/elitea-ng/elitea-deepwiki-engine-native` instead of
   `elitea-deepwiki` if you pull through a registry mirror.
5. Compose: drop `deploy/docker-compose.deepwiki-native.yml` from
   `STANDALONE_OVERLAY`; the base file now does what it did.

## runtime-redis removed: the platform runs without Redis — BREAKING

**Affects:** every Helm install whose values still carry a `runtimeRedis`
block (including `enabled: false`), every Argo CD Application built on those
values, every compose stack started from `deploy/docker-compose.standalone-full.yml`,
and anyone using the mixed `deploy/centry-hybrid` stack.

**What changed.**

- Nothing reads Redis any more. The runtime command bus is NATS JetStream
  (`docs/runtime-command-bus.md`), the Form sign-in state is in PostgreSQL
  (entry below), and live updates and canvas presence are on NATS. The plain
  `redis`/`elitea-valkey` was already gone.
- The chart no longer has `templates/runtimeRedis/` or a `runtimeRedis` values
  block, and it **refuses to render** a values file that still sets
  `runtimeRedis` at all (`runtimeRedis.enabled`, `.materialSecretName`,
  `.persistence`, `.bootstrap`, `.service`, … — any key under it), with a
  message naming this entry. The worker keys `worker.runtime.redisUrl`,
  `redisStream`, `redisGroup`, the `redis_*` limits, `main.runtime.redis` and
  an auth document `redis:` block were already refused.
- Compose: the `runtime-redis` service, its `standalone_runtime_redis` material
  volume, its `standalone_runtime_redis_data` AOF volume and
  `deploy/runtime/redis.conf` are gone.
- `deploy/scripts/gen-runtime-certs.sh` no longer mints `redis-server.crt`/`.key`,
  `redis-users.acl` or the `redis-{producer,worker,bootstrap}-password` files,
  and deletes them (and a leftover `redis-auth-password`) from an existing
  `deploy/certs/runtime/` tree in place. Nothing else in the tree is rotated.
  The runtime CA stays: it signs elitea-main's control/output/content listeners
  and the worker's workload identity.
- `deploy/centry-hybrid` is **retired** (owner decision: retire, don't port).
  It depended on the private legacy centry repository, and its runtime still
  wrote the Redis command-bus fields both workers refuse. The `hybrid:*` Task
  targets, the cutover-rehearsal suite and the Centry-profile reliability
  harnesses (`TestExistingComposeIndexReliability`, the issue #5681
  production-scale gate) went with it. `deploy/scripts/migrate-artifacts.sh`
  (the libcloud → object-store copy, formerly under the hybrid's `scripts/`)
  is kept; `deploy/ARTIFACT_CUTOVER.md` describes its use.

**What to do.**

1. Delete the `runtimeRedis` block from every values file (and from Argo CD
   `valuesObject`/`helm.values`). The render fails until you do.
2. After the upgrade, delete what the old release or your own tooling left
   behind. A `helm upgrade` removes the chart-owned Deployment, Service and
   ConfigMap; the data PVC always survives (the chart annotated it
   `helm.sh/resource-policy: keep`), as does anything an Argo CD
   `Prune=false` or a storage-class retain policy kept, and the material
   Secret was always yours:

   ```bash
   kubectl -n <ns> delete deployment,service elitea-runtime-redis --ignore-not-found
   kubectl -n <ns> delete configmap elitea-runtime-redis-config --ignore-not-found
   kubectl -n <ns> delete pvc elitea-runtime-redis-data --ignore-not-found
   kubectl -n <ns> delete secret elitea-runtime-redis-material --ignore-not-found
   ```

   (Names are the chart defaults; use your `runtimeRedis.service.name` /
   `fullnameOverride` if you changed them.) Then release the PersistentVolume
   if its reclaim policy is `Retain`.
3. Drop any `redis-*` keys from the runtime material Secret
   (`main.runtime.material.secretName`) and the auth material Secret if they
   are still there. Nothing reads them, and they are live credentials to a
   server that no longer exists.
4. Compose: `podman compose -f deploy/docker-compose.standalone-full.yml down`
   with `--remove-orphans`, then `podman volume rm <project>_standalone_runtime_redis
   <project>_standalone_runtime_redis_data` (the second exists only on stacks
   that ran the AOF-persistent runtime-redis).
   Re-run `deploy/scripts/gen-runtime-certs.sh` (or `task standalone:up`,
   which runs it) to prune the retired files from `deploy/certs/runtime/`.
5. Mixed-deployment users: there is no in-repo replacement for
   `deploy/centry-hybrid`. Move to the Helm chart or the standalone stack.

## NATS is secured (mTLS, one account per plane) and moves into the platform namespace — BREAKING; budget counters reset

**Affects:** every Kubernetes deployment of `deploy/helm/nats`,
`deploy/helm/nats-bootstrap` and `deploy/helm/elitea`, and every Argo CD
install from `deploy/argocd`. Compose is not affected unless you opt into
`deploy/docker-compose.nats-secure.yml`.

**What changed.**

- The NATS server requires TLS and a client certificate on 4222. Each client's
  certificate carries a URI SAN (`spiffe://elitea.internal/nats/<identity>`)
  that maps to a user (`verify_and_map`); there is no anonymous or password
  access. All URLs are `tls://…` with no credential.
- Users live in one NATS account per plane — `MAIN` (elitea-main: presence
  KV, presence subject), `GATEWAY` (gateway and scheduler: budget and
  rate-limit counters, write-behind deltas, cooldown KV) and `RUNTIME`
  (reserved for the command bus). Only GATEWAY's per-project soft-alert
  subject crosses (into MAIN).
- Canvas presence is published on `elitea.events.project.<id>.presence`
  (was `gateway.events.project.<id>.events`); the SSE route accepts each
  event type only from its own subject.
- The `nats-bootstrap` Job creates every stream, KV bucket and the
  scheduler's `budget-writeback` consumer, once per account with a
  per-account certificate that exists only during the install (a one-hour
  hook Certificate). The scheduler only binds, and keeps retrying until the
  consumer exists (`GET /readyz/budget-writeback`). Its
  `BUDGET_WRITEBACK_ACK_WAIT` / `BUDGET_WRITEBACK_MAX_DELIVER` moved to the
  bootstrap chart's `deltas.writeback.ackWait` / `maxDeliver`.
- In HA, routes use their own route CA and certificate.
- An ingress NetworkPolicy admits 4222 only from elitea-main,
  elitea-llm-gateway, elitea-scheduler and nats-bootstrap pods.
- The Argo CD sample installs NATS and its bootstrap into **`elitea`** (the
  platform's namespace) instead of `elitea-gateway`, because the NATS CA
  Issuer is namespaced and signs the platform's client certificates.

**What this resets.** Moving the NATS Application to another namespace
creates a new StatefulSet with a **new, empty JetStream volume**; nothing is
migrated (greenfield, by decision). After the switch:

- budget counters and rate-limit windows start at zero for the current
  period, so budget **enforcement** for the period restarts from zero (spend
  already written back to `gateway.llm_budget_accumulators` stays, and the
  budget screens keep showing it);
- write-behind deltas the scheduler had not drained yet are lost;
- the soft-alert cooldown resets, so an 80% alert can fire once more;
- canvas presence rosters reset (they rebuild within one heartbeat).

**What to do.**

1. Install cert-manager (a prerequisite now). Optionally install
   cert-manager approver-policy and disable cert-manager's built-in approver;
   the NATS chart then renders policies that pin what its issuers sign
   (`security.approverPolicy`).
2. Restrict `create` on cert-manager `Certificate`/`CertificateRequest` and
   read on `Secret` in the NATS namespace to the deployer and operators: those
   rights can mint any NATS identity (`deploy/helm/elitea/NATS.md`, "Who can
   mint a NATS identity"; the `deploy/helm/nats` chart this entry names is now
   bundled into `deploy/helm/elitea`).
3. Install NATS, `nats-bootstrap` and the platform into one namespace (or use
   a `ClusterIssuer`; the platform chart refuses a different `nats.namespace`
   with the namespaced Issuer). With Argo CD, sync the updated
   `deploy/argocd` Applications; the bootstrap runs as a PostSync hook.
4. Optional, to keep the undrained deltas: before switching, scale the LLM
   gateway to zero, wait until the old NATS's `budget-writeback` consumer
   shows no pending messages, then switch.
5. Delete what the old install left in `elitea-gateway`: the JetStream PVC
   (`elitea-nats-js-elitea-nats-0`, and `-1`/`-2` in HA) is not pruned by
   Argo CD, and the namespace itself if nothing else uses it.
6. Remove `BUDGET_WRITEBACK_ACK_WAIT` / `BUDGET_WRITEBACK_MAX_DELIVER` from
   any scheduler values override (they are ignored), and set
   `deltas.writeback.*` on the bootstrap if you had tuned them.

## Form sign-in state moved from Redis to PostgreSQL; auth document v2 — BREAKING

**Affects:** every deployment that sets `ELITEA_AUTH_CONFIG_FILE` (Helm:
`main.fileConfig.authConfig.enabled: true`), whether Form sign-in is on or off.

**What changed.**

- The Form graph's sign-in state — browser sessions, one-time login
  transactions and the attempt limiter — is in PostgreSQL now, in the
  `elitea_auth` tables of shared migration 0153. The auth Redis client and the
  `auth` Redis ACL user are gone, and readiness no longer checks Redis.
- The authentication document schema is `elitea.auth.form.v2`. It has no
  `redis:` block, and the attempt key moved to `credentials.attempt_key_file`.
  A `elitea.auth.form.v1` document is **refused**: elitea-main stops at boot,
  `elitea-auth-material` stops at pod start, and the chart refuses to render.
- The material is three files: `attempt_key_file`, `pat_signing_key_file` and
  `users_json_file`. The Redis password and the Redis CA are not read.
- A pre-login session (between the login page and the password submit) lives
  five minutes. An authenticated session still lives `cookie.lifetime_seconds`.
- elitea-scheduler deletes expired sign-in rows, including the OIDC and SAML
  `browser_sessions` rows that nothing deleted before.

**What to do.**

1. In the authentication document: set `schema_version: elitea.auth.form.v2`,
   delete the `redis:` block, and add
   `credentials.attempt_key_file` with the value of the old
   `redis.attempt_key_file`.
2. Run the migrations (the chart's migration Job does) before the new
   elitea-main starts.
3. Optional: drop the `redis-auth-password` and Redis CA keys from the auth
   material Secret, and the `user auth` line from the runtime Redis ACL file
   (the runtime Redis itself is removed since; see the entry above).
   An install still works with them present; nothing reads them.

People signed in through Form sessions are signed out once. OIDC and SAML
sessions are not affected.

## Form (username/password) sign-in is off by default — BREAKING

**Affects:** every deployment whose people sign in through the Form plane —
the local users in the Form users JSON file named by the
`ELITEA_AUTH_CONFIG_FILE` document (Helm: `main.fileConfig.authConfig`) — on a
deployment with no OIDC or SAML sign-in configured. Deployments that sign in
through OIDC or SAML are not affected: Form sign-in was already unmounted
there.

**What changed.** The local username/password sign-in is now controlled by
one switch, `ELITEA_FORM_LOGIN_ENABLED` (Helm: `main.env.ELITEA_FORM_LOGIN_ENABLED`),
and it is **off unless set to `true`**. Sign-in is meant to be OIDC or SAML,
with SCIM provisioning. With the switch off:

- the authentication document is still read and still required where it was
  — the gateway edge, personal access token validation and the runtime's
  forwarded-identity check depend on it;
- no `/auth/form/*` route and no Form `/auth/login` page is mounted, and the
  Form handler holds no users, so no configured password is accepted
  anywhere. With OIDC or SAML configured, `/auth/login` is the SSO sign-in
  page; with neither, it answers 404 and elitea-main logs
  `no browser sign-in is enabled` at start-up;
- a Form session created before the upgrade stops authorizing at once
  (people signed in with a Form password are signed out);
- configured Form users are ignored, and elitea-main logs that at start-up
  (`Form sign-in is disabled …; the configured Form users are ignored`).

Any value other than `true`/`false`/`1`/`0` (any letter case) or empty stops
the boot.
OIDC, SAML and SCIM behave exactly as before.

**What to do.**

- *You sign in with Form users and want to keep doing so:* set
  `ELITEA_FORM_LOGIN_ENABLED=true` before upgrading
  (`values-auth-minimal.yaml` already does). Read the next entry too: every
  Form user now needs a real email address.
- *You sign in with OIDC or SAML:* nothing to do. If your authentication
  document's `identity.initial_global_admins` lists Form logins, they no
  longer match anyone; list `oidc:<sub>`, `saml:<nameid>` or
  `email:<address>` entries instead.

**Making the first administrator of a fresh install** without Form sign-in:

- *OIDC:* set `OIDC_ISSUER_URL` (and the client settings) and name yourself
  in `identity.initial_global_admins` or `ELITEA_INITIAL_GLOBAL_ADMINS` —
  `email:<you@example.com>` works when your identity provider marks the
  address verified, `oidc:<sub>` always does. Your first OIDC sign-in receives
  the administration role.
- *SAML only:* a SAML identity provider is configured in the admin console
  (Configuration › Authentication), so it needs an administrator first, and
  there is no environment variable that configures SAML. Either bootstrap
  through OIDC as above, or, once: set `ELITEA_FORM_LOGIN_ENABLED=true` with
  one Form user (with an `email`) listed in `identity.initial_global_admins`,
  sign in, author the SAML provider and add your `saml:<nameid>` to the
  list, then set `ELITEA_FORM_LOGIN_ENABLED=false` and restart — the restart
  also mounts the SSO sign-in page, which is decided at boot.

## Form users must have a real email address

**Affects:** deployments that sign people in through the Form plane (the
local username/password users in the Form users JSON file named by
`provider.form.users_json_file` in the `ELITEA_AUTH_CONFIG_FILE` document).

**What changed.** A Form user's address is now read from the user's
top-level `email` field — the field the admin schema writes — and, when that
is absent, from `attributes.email`. Before, the top-level field was accepted
and ignored, and a user with no `attributes.email` signed in as
`<login>@centry.user`. That domain is the platform's reserved system-identity
domain (`system@centry.user`, `system_user_<n>@centry.user`): the Users page,
analytics, budgets and SCIM all treat an address in it as a platform account,
so the person disappeared from those surfaces.

No address is synthesized any more, on any sign-in plane. A Form user with no
address, with a top-level `email` that is not a usable address (it contains
whitespace or control characters), or with an address in `@centry.user` is a
configuration error:

- the users file still loads, so one bad entry does not lock the others out;
- elitea-main logs a warning at start-up naming the affected logins
  (`Form users have no usable email address and will be refused at sign-in`);
  `elitea-auth-validate` prints the count;
- that user's sign-in is refused with the ordinary "sign-in failed" page, and
  the server log says why (`Form sign-in refused: the Form user has no email
  address configured`, with the login).

**What to do before upgrading.** Give every Form user a real address:

```json
{"users": [
  {"login": "alice", "password": "…", "email": "alice@example.com"}
]}
```

**Accounts already created as `<login>@centry.user`.** Nothing is rewritten
automatically: which real address belongs to each person is your decision,
and changing an address changes how the account matches future sign-ins. At
start-up elitea-main logs a warning listing such accounts
(`sign-in accounts have a synthesized address in the reserved @centry.user
domain`, with their provider references). An existing account keeps signing
in — it is found by its provider link, not by its address — so you can fix
the rows at your own pace:

1. List them:

   ```sql
   SELECT u.id, u.email, p.provider_ref
   FROM public.auth_core__user AS u
   JOIN public.auth_core__user_provider AS p ON p.user_id = u.id
   WHERE lower(u.email) LIKE '%@centry.user'
     AND lower(u.email) <> 'system@centry.user'
     AND lower(u.email) NOT LIKE 'system\_user\_%@centry.user';
   ```

2. For each person, set the address you configured for them in the users
   file, after checking that no other account already holds it:

   ```sql
   UPDATE public.auth_core__user SET email = 'alice@example.com' WHERE id = <id>;
   ```

The warning stops once no such account remains.
