# Governed credential profile: scale (synthetic)

ADR-0039 P6b, issue #1124 work item 4. One synthetic run. The numbers hold for
the dimensions and the machine below and for nothing beyond them. They are not
a production throughput claim.

## Dimensions

- 300 teams (projects) x 60 calling applications, 1 agent per application,
  2 grants per agent.
- 18,000 agents, agent identity bindings, external agent identities, sign-in
  assertions, delegations and mandates.
- 36,000 credential references, target bindings, agent toolkit bindings and
  credential use grants.
- 1 policy revision (`scale-r1`).
- Bindings, identities, delegations, mandates and grants are written through
  the production stores (126,000 audit rows). Target bindings, credential
  references and toolkit bindings have no production writer yet and are
  bulk-copied.

## Machine

- Apple M4 Pro, 14 cores, 48 GB. The Go test process ran on the host (Go 1.26.9).
- PostgreSQL 18.6 in a podman VM (applehv, 7 vCPUs, 2 GiB), stock settings:
  `synchronous_commit=on`, `max_connections=100`.
- NATS 2.12.0 (`-js`) in the same VM.

## Real and stubbed

Real, against PostgreSQL: the credential broker, the AND-composed decider, the
admission transaction (`AdmitAgentExecution` with the governed pin, mandate
bind and security context), the governed reference resolver, the epoch reads,
the refresh coordinator and its fenced lease, the provider OAuth consent and
token source, the security audit appender and relay, the epoch hub over NATS.

Stubbed: the RBAC and entity-access gates (always allow), the secret backend
(in memory), the identity provider (the fake provider), the Jira token
endpoint (an in-process rotating fake, 40 ms per refresh, single-use refresh
tokens). Each of the 3 replicas is its own 16-connection pool and broker in
one process.

## Results

| Measurement | Load | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| Admission, legacy (#999 method) | 5 bursts x 100 starts, 40 concurrent, 3 replicas | 29.5 ms | 37.4 ms | 41.2 ms | 47.2 ms |
| Admission, governed | same, plus resolve, freeze, pin, mandate bind, security context | 41.6 ms | 61.1 ms | 66.0 ms | 67.6 ms |
| Redeem, spread over 276 teams | 6,000 redemptions, 32 workers, 3 replicas, 1,500 runs, 972/s | 30.9 ms | 43.6 ms | 47.0 ms | 70.2 ms |
| Redeem, first of a run | 1,500 of the above (opens the agent session) | 41.1 ms | 46.6 ms | 60.3 ms | 70.2 ms |
| Redeem, repeat | 4,500 of the above | 29.9 ms | 34.0 ms | 36.0 ms | 43.6 ms |
| Redeem, one team | 3,000 redemptions, 32 workers, 60 runs of one team, 407/s | 63.8 ms | 191.6 ms | 277.5 ms | 381.8 ms |
| Refresh contention | 5 rounds x 60 concurrent redemptions of one expiring grant, 3 replicas | 124.1 ms | 189.5 ms | 207.4 ms | 209.5 ms |
| Revocation to deny | 50 trials: 10 each of grant, mandate, binding, delegation, credential reference | 4.2 ms | 5.2 ms | 5.8 ms | 5.8 ms |
| Audit outbox lag | the redeem loads, relay at defaults (1 s, batch 500) | 471 ms | 879 ms | 951 ms | 986 ms |
| NATS invalidation fan-out | 500 epoch changes, 2 hubs | 0.21 ms | 0.30 ms | 0.37 ms | 0.51 ms |

Invariants the run asserts (it fails otherwise):

- every redemption in the redeem and refresh loads is permitted;
- each refresh round makes exactly one provider refresh call (5 in total) and
  no `invalid_grant`;
- no redemption that starts after a revocation commits is permitted, on a
  replica other than the writer;
- the audit backlog drains to zero and every outbox row is in the durable store
  with the same hash;
- every governed admission left its pin and security context.

Observations:

- The seed's 126,000 audit rows drained at about 29,000 rows/s before the load.
  The load's backlog peaked at 2,158 rows; the outbox lag is set by the relay
  interval.
- A team's audit events are one hash chain, and every append holds the chain
  head row until commit. One team's redemptions therefore serialise on commit
  time: about 400/s on this machine, with p99 277 ms. Load spread across teams
  did not show it.
- Revocation to deny is about one redemption's duration, because the broker
  re-reads the epochs on every redemption; it does not wait for the fan-out.

## Limit: one audit chain per team

Owner decision on #1480: keep one hash chain per team and publish the ceiling.
Every governed decision and redemption appends to its team's chain, and the
append locks the chain's head row (`elitea_runtime.security_audit_heads`) until
the transaction commits. One team's governed credential use is therefore
serial on commit latency. Measured above, PostgreSQL 18 with stock settings:

| Load | Throughput | p99 |
|---|---|---|
| Spread across teams | about 970 redemptions/s | 47 ms |
| One team | about 400 redemptions/s (the ceiling) | 277 ms |

The ceiling moves with commit latency (disk, `synchronous_commit`, network to
PostgreSQL), not with replicas: adding Main replicas does not raise it.
Different teams do not contend.

Watch `elitea_security_audit_head_lock_wait_seconds{partition,result}`, the
time each append waited for its chain head. `partition` is `tenant` or
`platform`; there is no tenant label. A p99 that climbs towards the commit
time of a redemption means one or more teams are at the ceiling. The
broker's `elitea_credential_redeem_duration_seconds` then rises with it.

## Reproduce

```bash
podman run -d --rm --name scale-pg -e POSTGRES_PASSWORD=pw -p 55434:5432 docker.io/library/postgres:18
podman run -d --rm --name scale-nats -p 54222:4222 docker.io/library/nats:2.12.0-alpine -js
cd services/elitea-main
ELITEA_GOVERNED_SCALE=1 \
ELITEA_GOVERNED_SCALE_REPORT=/tmp/governed-scale-report.txt \
ELITEA_TEST_DATABASE_URL='postgres://postgres:pw@localhost:55434/postgres?sslmode=disable' \
ELITEA_TEST_NATS_URL=nats://127.0.0.1:54222 \
go test -v -count=1 -run '^TestGovernedScale$' -timeout 30m ./internal/credentialgovernance/scale/
podman stop scale-pg scale-nats
```

`ELITEA_GOVERNED_SCALE_TEAMS`, `ELITEA_GOVERNED_SCALE_APPS`,
`ELITEA_GOVERNED_SCALE_AGENTS` and `ELITEA_GOVERNED_SCALE_GRANTS` change the
dimensions; `ELITEA_GOVERNED_SCALE_REDEEMS` the spread redeem count. The
workflow `.github/workflows/ci-governed-scale.yml` runs the same nightly and on
dispatch against a PostgreSQL 18 service container and uploads the report as
the `governed-scale-report` artifact.

## Raw report

```text
governed-scale-report v1
env host goos=darwin goarch=arm64 cpus=14 go=go1.26.9 postgres=18.6 synchronous_commit=on max_connections=100
dimensions synthetic teams=300 calling_apps_per_team=60 agents_per_app=1 grants_per_agent=2 agents=18000 grants=36000
seed fixture seconds=79.5
rows elitea_identity.credential_policy_revisions count=1
rows elitea_identity.external_agent_identities count=18000
rows elitea_identity.agent_identity_bindings count=18000
rows elitea_identity.signin_assertions count=18000
rows elitea_identity.delegations count=18000
rows elitea_identity.mandates count=18000
rows elitea_identity.target_bindings count=36000
rows elitea_identity.credential_references count=36000
rows elitea_identity.agent_toolkit_bindings count=36000
rows elitea_identity.credential_use_grants count=36000
rows elitea_runtime.security_audit_outbox count=126000
latency admission.legacy n=500 p50_ms=29.470 p95_ms=37.431 p99_ms=41.227 max_ms=47.179 bursts=5 burst=100 concurrency=40 replicas=3
latency admission.governed n=500 p50_ms=41.636 p95_ms=61.082 p99_ms=65.981 max_ms=67.587 bursts=5 burst=100 concurrency=40 replicas=3
throughput audit.relay_drain rows=126000 rows_per_second=29330
latency redeem.all n=6000 p50_ms=30.898 p95_ms=43.606 p99_ms=46.998 max_ms=70.188 replicas=3 workers=32 runs=1500 teams=276 per_second=972
latency redeem.first_of_run n=1500 p50_ms=41.095 p95_ms=46.645 p99_ms=60.318 max_ms=70.188
latency redeem.repeat n=4500 p50_ms=29.860 p95_ms=33.993 p99_ms=35.999 max_ms=43.603
latency redeem.one_team n=3000 p50_ms=63.826 p95_ms=191.616 p99_ms=277.453 max_ms=381.819 replicas=3 workers=32 runs=60 teams=1 per_second=407
latency audit.outbox_lag n=20864 p50_ms=471.097 p95_ms=878.910 p99_ms=951.142 max_ms=986.434 relay_interval_ms=1000 relay_batch=500 max_backlog=2158
latency refresh.contended_redeem n=300 p50_ms=124.072 p95_ms=189.503 p99_ms=207.439 max_ms=209.485 callers=60 rounds=5 replicas=3 provider_refresh_calls=5 invalid_grant=0 provider_delay_ms=40
latency revocation_to_deny.grant n=10 p50_ms=3.469 p95_ms=4.146 p99_ms=4.146 max_ms=4.146 trials=10
latency revocation_to_deny.mandate n=10 p50_ms=4.214 p95_ms=4.677 p99_ms=4.677 max_ms=4.677 trials=10
latency revocation_to_deny.binding n=10 p50_ms=3.646 p95_ms=4.243 p99_ms=4.243 max_ms=4.243 trials=10
latency revocation_to_deny.delegation n=10 p50_ms=4.801 p95_ms=5.340 p99_ms=5.340 max_ms=5.340 trials=10
latency revocation_to_deny.credential_reference n=10 p50_ms=4.801 p95_ms=5.796 p99_ms=5.796 max_ms=5.796 trials=10
latency revocation_to_deny.all n=50 p50_ms=4.175 p95_ms=5.174 p99_ms=5.796 max_ms=5.796
latency invalidation.nats n=500 p50_ms=0.211 p95_ms=0.296 p99_ms=0.365 max_ms=0.513 measured=yes changes=500 replicas=2
```
