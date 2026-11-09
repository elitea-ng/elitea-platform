---
title: "Scale-out readiness: optional KEDA and prometheus-adapter, an all-component connection budget, drain and grace periods"
issue: 1390
date: 2026-10-22
order: 25
---

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
