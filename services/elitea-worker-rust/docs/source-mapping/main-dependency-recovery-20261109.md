# Main survives a PostgreSQL or NATS recovery (FU-275), 2026-11-09

Main used to exit when PostgreSQL went into recovery, and compose never started it again, so every run on the stack
stopped. Main now stays up through a database or NATS outage, reports it, and resumes when the dependency is back.
Compose also restarts any long-running service that exits.

Plan and review: `handoffs/point5-wave1-20261008/fu275/PLAN.md` and `REVIEW-PLAN.md`. The decisions were Q1 = A1
(supervised runners), Q2 = R2 (503 for the database, `degraded` for NATS), Q3 = B1 (fail-fast boot plus restart
policy), Q4 = every long-running compose service. Review conditions C1-C6 and note N3 are covered below.

## Business behaviour

Nothing here comes from the current platform: Pylon has no equivalent of the runtime runner set. The behaviour is
new and follows the delivery gate's resilience rule (bounded, typed, readable, never a silent hang).

**What happened on main** (live reproduction on a disposable stack from `product-real-models-main-ce2756fad.dump`,
images `main-ce2756fad-verify`):

| Fault | Before |
|---|---|
| `docker restart postgres` | Main exited within a second: `production runtime failed: runtime outbox publisher: publisher 2: retire expired toolkit discovery: … server login has been failing, cached error: the database system is shutting down (server_login_retry) (SQLSTATE 08P01)`. Compose left it stopped. |
| Starting Main 19 s later | Boot refused (`credential governance refused boot: … cached error …`): PgBouncer serves its cached login failure for `server_login_retry`, which `deploy/runtime/pgbouncer.ini` does not set, so the default 15 s applies (N1, not tuned here). |
| `docker restart nats` | Main survived; both connections reconnected in 0.5 s. |
| `docker stop nats` for 50 s | Main survived, but readiness answered `ready` throughout, so the outage was invisible. |

The field messages (fanpar 2026-11-02, -03, -05; fu250 2026-11-03) read `runtime publisher stopped without an owning
cancellation`. That is the same runner when the database hangs instead of refusing: its sweep derives its own 30 s
timeout (`services/elitea-main/internal/infra/db/repos/toolkit_discovery_artifacts.go:322`), and the publisher set
treated any context error as a cancellation, so the runner's name and the cause were lost.

**Root cause.** One runner of 21, `currentToolkitDiscoveryRuntime`, returned the first database error. The set ended
Main on the first runner to return. An audit of the other 20 runners found that each one logs and retries; none of
them returns on a database or NATS error, and the private gRPC servers touch neither.

**Not ported or not done here:**
- No boot-time dependency wait (B1). Boot stays fail-fast with a readable reason, and the restart policy retries it.
- The other runners' per-cycle ERROR lines during an outage are unchanged; they are an FU-299 test case (N2).
- Liveness still never detects a hung Main: FU-301.

## Changed paths

| Path | What |
|---|---|
| `services/elitea-main/internal/infra/depfailure/depfailure.go:29-90` | `Transient(err, owner)`: the closed list of failures that heal on their own. SQLSTATE class 08 (PgBouncer's `08P01` login retry included), `57P01`/`57P02`/`57P03`, `53300`; refused, reset or unresolved connections; a broken pooled connection; NATS `ErrTimeout`, `ErrNoResponders`, `ErrReconnectBufExceeded`; a deadline only while the owner context is alive (`:72`). Excluded on purpose (C1): a closed pgx pool, NATS `ErrConnectionClosed`, `ErrNoServers` and `ErrConnectionReconnecting` (each only after an explicit Close, a drain or giving up; with `MaxReconnects(-1)` never on a live reconnecting connection), a wrong password or missing database inside a connect error, serialization failures. |
| `services/elitea-main/internal/infra/depfailure/depfailure.go:94` | `Describe(err)`: one readable cause, with the SQLSTATE and server message, or the kind of network failure. It never includes the user, database, host, port or SQL. |
| `services/elitea-main/internal/runtimecomposition/publishers.go:17-26` | Restart bounds: `runnerRestartMinDelay` 1 s, `runnerRestartMaxDelay` 30 s, `runnerCleanRunDuration` 1 min (the "clean minute", C2). |
| `services/elitea-main/internal/runtimecomposition/publishers.go:83-108` | `newPublisherSet` flattens nested sets and keeps each runner's name. `named(...)` labels every runner at its composition site (`composition.go`); an unnamed runner is named after its type. |
| `services/elitea-main/internal/runtimecomposition/publishers.go:184-215` | `supervise`: restarts a runner only after its `Run` returned, so there is never a second copy. It restarts only on `depfailure.Transient` (`:196`). The failure count carries across restarts and resets only after a clean minute (`:200`). The wait honours the owner context (`:213`). Every restart logs one WARN: "Main restarts the <runner> runner after a dependency failure, in <delay>", with runner, attempt, delay and cause. Any other stop returns `runtime runner "<name>" stopped: <cause>` and logs "The <runner> runner stopped and Main is exiting: <cause>" (`:248`). |
| `services/elitea-main/internal/runtimecomposition/publishers.go:117` | `DegradedRunners()`: runners restarted within the last clean minute. |
| `services/elitea-main/internal/runtimecomposition/toolkit_discovery_runtime.go:86-110` | The toolkit discovery housekeeping runner logs each failed pass ("…pass failed; retrying in <delay>") and backs off from 1 s to 30 s (the shared `runnerRestartDelay`); a clean pass returns to the 10 s interval. It returns only its owner's error. |
| `services/elitea-main/internal/runtimecomposition/runtime.go:63-79` | `Runtime.DegradedRunners()`; `Runtime.RuntimeNATSStatus()`, the client-side state with no round trip. |
| `services/elitea-main/internal/api/health/readiness.go:24-182` | `Check`, `Evaluate` and `Monitor`. Evaluate runs every check concurrently under one 2 s bound (`:24`, under Helm's 3 s and compose's 5 s probe timeouts). A failing critical check answers 503 `not_ready`; any other failing check answers 200 `degraded`. The body holds only states, never an error text. `Monitor.Observe` (`:134`) logs one WARN when a dependency is lost and one INFO with the outage length when it is back, per dependency, however many runners or probes saw the outage (C4). |
| `services/elitea-main/internal/api/health/handler.go` | `Deps.Checks` and `Deps.Monitor`. `Deps.DB` alone keeps the old single-database readiness. |
| `services/elitea-main/cmd/elitea-main/readiness_checks.go:40-100` | Main's checks. `db` (critical) pings every pool once and concurrently, through pool connections: the main pool and the runtime admission pool reach the same PostgreSQL, so one outage is one transition. `nats` reads `Conn.Status()` of the runtime and live-update connections. `runners` reports `DegradedRunners()` and logs nothing itself, because restarts have their own lines. |
| `services/elitea-main/cmd/elitea-main/main.go:1684,2134,2995-3010,3050-3058` | Pools collected for readiness; the monitor runs every 5 s (`readinessMonitorInterval`) until shutdown; the router's readiness uses the same checks. `poolChecker` is removed. |
| `deploy/docker-compose.standalone-full.yml`, `deploy/docker-compose.yml`, `deploy/docker-compose.e2e-standalone.yml`, `deploy/docker-compose.storage-test.yml`, `deploy/docker-compose.secretstore.yml` | `restart: unless-stopped` on every long-running service that lacked one: Main, Web, postgres, pgbouncer, nats, nats-health, rustfs, oidc-mock, otel-collector, the subapp hosts, qdrant, vector, openbao, azurite, fake-gcs-server. `restart: "no"` on `nats-bootstrap`, the only one-shot that lacked it. The Worker, the sandbox supervisor and the other daemons already had `unless-stopped`. |
| `services/elitea-main/tests/deployedge/compose_restart_policy_test.go` | The compose gate (C5); see Tests. |

## Tests

| Test | Proves | Red on main? |
|---|---|---|
| `internal/infra/depfailure/depfailure_test.go` `TestTransientKeepsOnlyFailuresThatHealOnTheirOwn` | 17 kept and 12 excluded cases, including a real refused pgx connect, a real closed pgx pool, and a deadline on both sides of the owner's state (C1). | New package. |
| `…/depfailure_test.go` `TestDescribeKeepsTheCauseAndDropsConnectionDetails` | No user, password, host, port or database name in any description, including a real `ConnectError`. | New. |
| `internal/runtimecomposition/publishers_supervision_test.go` | Restart without cancelling siblings; at most one copy over 51 runs; delays 1, 2, 4, 8, 16, 30, 30, 30 s for a runner failing right after each restart, back to 1 s only after a clean minute; delay bounds over 64 failures, including an out-of-contract jitter; owner cancel ends a 1 s restart wait at once; a fatal stop names the runner and the cause (non-transient, closed pool, nil); a runner's own timeout is a restart, never "stopped without an owning cancellation"; nested sets are flattened and named. | Yes: on main a runner's own `DeadlineExceeded` ended the set with the anonymous message, and any return ended Main. |
| `internal/runtimecomposition/toolkit_discovery_runtime_test.go` | Three PgBouncer `08P01` failures: three readable ERROR lines, waits of 1, 2, 4 s, then the 10 s interval; Run returns only the owner's cancellation. | Yes: on main Run returned the first error. |
| `internal/runtimecomposition/dependency_recovery_postgres_integration_test.go` `TestRuntimeRunnersSurviveADatabaseOutageAndLogEachTransitionOnce` | **Real PostgreSQL** behind an in-test TCP proxy, with the real toolkit discovery SQL on migrated shared schemas. A cut (refused dials, reset connections): the housekeeping runner backs off, a runner that returns its first database error is restarted, readiness is `not_ready`, and the set never stops. After the restore, both resume. Exactly **one** "Main lost its database" WARN and **one** "Main reached its database again after …" INFO, and no cause carries the user or database name. | — |
| `…/dependency_recovery_postgres_integration_test.go` `TestRuntimeRunnersWaitOutADatabaseInRecoveryMode` | A fake server on `pgproto3` answers every login with `57P03`: the runner is restarted with cause "the database system is in recovery mode (SQLSTATE 57P03)", reported degraded, and readiness is `not_ready`. Runs without any environment. | — |
| `internal/api/health/readiness_test.go` | Readiness table: ready; database down 503; NATS reconnecting 200 `degraded`; a runner in backoff 200 `degraded`; both down 503; an empty probe counts as unavailable. No error text in any body. Three 300 ms checks answer in about 300 ms (concurrent); a hung check answers at the 2 s bound, which is below 3 s. The monitor logs one loss and one recovery line per dependency over 20 failing evaluations, with the outage length; the runners check logs nothing. | New. |
| `cmd/elitea-main/readiness_checks_test.go` `TestMainReadinessFollowsARealNATSRestart` | **Real nats-server** killed and started again under a connection with Main's reconnect options: 200 `degraded` with `nats: reconnecting`, then `ready`; one loss and one recovery line. Needs `ELITEA_TEST_NATS_SERVER_BIN` (CI sets it). | Yes: on main readiness had no NATS check. |
| `cmd/elitea-main/readiness_checks_test.go` (two more) | An unreachable pool gives 503; degraded runners give 200 `degraded` and no log line. | — |
| `tests/deployedge/compose_restart_policy_test.go` `TestEveryComposeServiceHasTheRestartPolicyItsLifetimeNeeds` | Reads every compose file under `deploy/`. A service another one waits on with `service_completed_successfully` is a one-shot and must say `"no"`; every other declared service must say `unless-stopped`. An overlay may inherit only from a base file (one that declares the database) that sets it. It has floors for Main, Web, Worker, sandbox, postgres, pgbouncer, nats, migrate and nats-bootstrap, and fails if it reads fewer than 30 services. | Yes: 11 long-running services in the standalone stack had no policy. |

Existing tests were not edited. Counts and skips for the run: see "Test run".

## Performance

- **Healthy path.** No new work on a request. The readiness monitor runs every 5 s: one `Ping` per pool (2 pools)
  and two `Conn.Status()` reads (`readiness_checks.go:40`). A probe adds the same; both are bounded by 2 s and run
  concurrently (`readiness.go:55`), measured at about 300 ms for three 300 ms checks.
- **Outage.** A failing runner retries at most every 30 s after the backoff ramp (1, 2, 4, 8, 16, 30 s), not every
  10 s or every cycle; a flapping runner cannot restart faster than its backoff (C2 test). Pings go through the pools'
  own connections, so readiness never adds connections beyond `MaxConns`.

## Durability

- Main holds no run state in memory that an outage could lose. Claims, leases, outbox rows and receipts stay in
  PostgreSQL, and the backoff only delays work that is already durable.
- A restarted runner starts from its durable state, as a process restart would; only one copy ever runs, so no
  durable row is processed twice by two copies (C2 test).
- Recovery rows: see "Recovery guarantee".

## Resilience

- Everything is bounded. Restart delay ≤ 30 s (`publishers.go:222`). Readiness ≤ 2 s. The monitor interval is 5 s.
  The clean-minute reset is 1 min. The classifier is closed (`depfailure.go:29-46`).
- Typed and readable: every transition and restart is one sentence with the runner or dependency and its cause; a
  fatal stop names the runner (`publishers.go:248`); `runtime runner "<name>" stopped: <cause>` replaces "publisher 2"
  and "stopped without an owning cancellation".
- No silent coercion: a permanent failure (closed pool, closed NATS connection, wrong password, missing database,
  programming error) still ends Main, by name, and compose restarts it.
- Cancellation: an owner cancel ends a restart wait at once (C2 test); `Monitor.Run` ends with its context.
- **Compose stays `healthy` while `degraded`**: the healthcheck runs `/elitea-main -healthcheck` against `/readyz`,
  which answers 200 for `degraded`. An operator sees `degraded` in the `/readyz` body (`checks.nats: "reconnecting"`,
  `checks.runners: "degraded"`) and in the WARN line "Main lost NATS and is reconnecting; …" (C3). A database outage
  answers 503, so compose marks Main `unhealthy` until it is back, without restarting it.
- Helm: readiness takes a replica out of the Service only for a database outage. Liveness is unchanged and
  dependency-free (FU-301 for the hung case).

## Security

- `rules/security.md` categories:
  - **Identity, authorization, injection, URL building, egress, supply chain:** not applicable; no route, query,
    identity or outbound call is added. `/readyz` stays loopback-only at the browser edge (`webedge.go:76`).
  - **Error and log leakage (applies):** the readiness body carries states only (table test with a cause containing a
    DSN, host and password); log causes go through `depfailure.Describe`, which drops the user, database, host and
    port (test with a real `ConnectError`); the real-PG test asserts that no restart line carries the user or database
    name.
  - **Secrets:** none added; the diff was scanned before each commit.
  - **Availability:** the change removes a single-fault outage; the restart policy cannot loop a failing migration
    (one-shots keep `"no"`, gated by the compose test).
- `/security-review` and `govulncheck`: see "Reviews".

## Recovery guarantee

| Component × phase | Class | Enforced by | Proved by |
|---|---|---|---|
| Main × PostgreSQL outage, admission | F then I | Admission writes in one transaction; a failure writes nothing; a retry with the same `question_id` returns the same execution (`repos/agent_execution_jobs.go:107-140`). Main stays up to answer (`publishers.go:184-215`). | Browser step 1 (N3) |
| Main × PostgreSQL outage, claimed run | R (database back within the 30 s lease), else R/F¹ via the existing Main × P02/P03/P10 recovery | Retryable `DEPENDENCY_UNAVAILABLE` (`repos/claims.go:544`, `control/server.go:864`); Main no longer exits | Browser step 1 |
| Main × PostgreSQL outage, outbox, output, settlement | I | Outbox backoff and exact-bytes republish (`execution/outbox_publisher.go:100-123`, `commandbus/jetstream.go:269`) | Real-PG proxy test; existing outbox tests |
| Main × NATS outage, admission and claimed run | I / R | Outbox; lease renewal over gRPC; reconnect for ever; readiness `degraded` | Real NATS test; browser step 2 |
| Main × process exit (compose) | R | `restart: unless-stopped` | Compose test; browser step 4 |

The platform matrix (`services/elitea-worker-rust/docs/recovery-guarantees.md` sections 1, 5, 8 and 9) carries
these rows. The old row "Main x PostgreSQL and NATS crash: pools reconnect" was not true on compose and is replaced.

## Browser

To be recorded.

## Reviews

To be recorded.

## Follow-ups

- FU-301: liveness never detects a hung Main (registered from C6).
- FU-299 test case: the other runners' per-cycle ERROR lines during a database outage (N2).
- N1: PgBouncer `server_login_retry` stays at the 15 s default; not tuned.
