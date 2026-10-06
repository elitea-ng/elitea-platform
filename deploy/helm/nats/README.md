# NATS JetStream — LLM gateway coordination substrate (BF0.2b)

The `elitea-llm-gateway` replicas coordinate all shared state through **NATS
JetStream** (design §8.1.1), *not* Redis/Valkey. NATS hosts:

| Asset | Kind | Purpose |
|-------|------|---------|
| `GATEWAY_BUDGET` | KV bucket | int64 nano-USD budget counters (`Nats-Incr`) |
| `GATEWAY_ALERT_COOLDOWN` | KV bucket | 80% soft-alert cooldown (`kv.Create` = SETNX-with-TTL) |
| `GATEWAY_BUDGET_DELTAS` | stream | write-behind deltas (subject `gateway.budget.delta`) drained by the `budget-writeback` consumer in `elitea-scheduler` |
| `gateway.events.*` | subject | EventBus (re-pointed from Redis pub/sub). `gateway.events.project.<id>.events` carries the gateway's `budget.soft_alert` AND elitea-main's domain events; elitea-main's project SSE stream subscribes to it (`ELITEA_EVENTS_NATS_URL`) |
| `ELITEA_CANVAS_PRESENCE` | KV bucket | elitea-main canvas presence rosters; created by elitea-main at startup, MaxAge 120s, History 1, file storage, replicas `ELITEA_EVENTS_NATS_REPLICAS` (default 1) |

There is **no `GATEWAY_CUTOVER` bucket** — the migration is big-bang with no
per-project cutover tracker.

## NATS Server version

**NATS Server 2.12.0+ is required** for the atomic `Nats-Incr` KV operation
(ADR-49). Both profiles pin `nats:2.12.0-alpine`. Do **not** downgrade — a
sub-2.12 server silently lacks `Nats-Incr` and budget enforcement breaks.

## Profiles (design §8.1.1)

These are values overlays for the upstream `nats` Helm chart
(`https://nats-io.github.io/k8s/helm/charts/`, chart `nats`). `Chart.yaml`
here pins the upstream chart as a dependency; `helm dependency build` vendors it
into `charts/`.

### scale-1 (`values-scale1.yaml`) — the deployment default

Single NATS node, KV buckets and stream created at `replicas = 1`, file storage.
HA is intentionally waived (no RAFT quorum). Correct and sufficient for a
single-gateway-replica deployment: the shared-counter design is still the durable
coordination substrate, and it is what keeps enforcement correct the moment a
second gateway replica is added. On node loss the un-persisted counter tail is
lost, but the write-behind keeps Postgres seconds-fresh so recovery
reconciliation rebuilds the counter. Pair with `LLM_BUDGET_EXPECTED_REPLICAS=1`.

```bash
helm dependency build deploy/helm/nats
helm upgrade --install nats deploy/helm/nats \
  -n elitea-gateway --create-namespace \
  -f deploy/helm/nats/values-scale1.yaml
```

### HA (`values-ha.yaml`) — opt-in

3 nodes, KV buckets and stream created at `replicas = 3` so every write is
RAFT-quorum-replicated (survives any single-node loss). A 1-replica store has no
quorum, so multi-node HA **requires** `replicas >= 3` (odd count for quorum; use
5 for larger fault tolerance). HA operators MUST also set the gateway's
`LLM_BUDGET_EXPECTED_REPLICAS` to the real gateway replica count and pass
`--set nats.replicas=3` to the bootstrap job (see `../nats-bootstrap`).

```bash
helm upgrade --install nats deploy/helm/nats \
  -n elitea-gateway --create-namespace \
  -f deploy/helm/nats/values-ha.yaml
```

## KV / stream creation

The buckets and stream are **not** created by this chart — they are created by
the `nats-bootstrap` post-install Job (`../nats-bootstrap`), which runs the
`nats` CLI with the correct `duplicate_window` + retention limits and a
`--replicas` matching the profile. See that chart's README.

## Verifying (runbook §2a / pre-flight step 2)

```bash
nats --context gateway server report jetstream            # HA: RAFT leader per group; scale-1: 1 server
nats --context gateway kv ls | grep -E 'GATEWAY_BUDGET|GATEWAY_ALERT_COOLDOWN'
nats --context gateway stream info GATEWAY_BUDGET_DELTAS --json | jq '.config.num_replicas'  # 1 | >=3
```
