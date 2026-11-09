---
title: "Scaling for every component: Inventory on N replicas, fleet-wide DeepWiki slots, autoscalers on by default"
issue: 1398
date: 2026-10-22
order: 26
---

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
