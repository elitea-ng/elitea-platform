# Main survives a PostgreSQL or NATS recovery (FU-275), 2026-11-09

Main used to exit when PostgreSQL went into recovery, and compose never started it again, so every run on the stack
stopped. Main now stays up through a database or NATS outage, reports it, answers Workers "retry" instead of "not
authenticated", and resumes when the dependency is back. Compose restarts any long-running service that exits.

Plan and reviews: `handoffs/point5-wave1-20261008/fu275/PLAN.md` and `REVIEW-PLAN.md`. The decisions were:
- Q1 = A1: supervised runners.
- Q2 = R2: 503 for the database, `degraded` for NATS.
- Q3 = B1: fail-fast boot plus a restart policy.
- Q4: every long-running compose service.

Review conditions C1–C6 and note N3 are covered below. So are the six conditions on the workload-authorization change
(the "auth path" section).

## Business behaviour

Nothing here comes from the current platform, which has no runtime runner set. The behaviour is new and follows the
delivery gate's resilience rule (bounded, typed, readable, never a silent hang).

**What happened on main** (live reproduction on a disposable stack from `product-real-models-main-ce2756fad.dump`,
images `main-ce2756fad-verify`):

| Fault | Before |
|---|---|
| `docker restart postgres` | Main exited within a second: `production runtime failed: runtime outbox publisher: publisher 2: retire expired toolkit discovery: … server login has been failing, cached error: the database system is shutting down (server_login_retry) (SQLSTATE 08P01)`. Compose left it stopped. |
| Starting Main 19 s later | Boot refused (`credential governance refused boot: … cached error …`). PgBouncer serves its cached login failure for `server_login_retry`. `deploy/runtime/pgbouncer.ini` does not set it, so the default 15 s applies (N1, not tuned here). |
| `docker restart nats` | Main survived; both connections reconnected in 0.5 s. |
| `docker stop nats` for 50 s | Main survived, but readiness answered `ready` throughout, so the outage was invisible. |

The field messages (fanpar 2026-11-02, -03 and -05; fu250 2026-11-03) read "runtime publisher stopped without an
owning cancellation". That is the same runner when the database hangs instead of refusing:
- its sweep derives its own 30 s timeout (`services/elitea-main/internal/infra/db/repos/toolkit_discovery_artifacts.go:322`);
- the publisher set treated any context error as a cancellation;
- so the runner's name and the cause were lost.

**Root causes.**
1. **The runner set.** One runner of 21, `currentToolkitDiscoveryRuntime`, returned the first database error, and
   the set ended Main on the first runner to return. An audit of the other 20 found that each logs and retries; none
   returns on a database or NATS error. The private gRPC servers touch neither.
2. **The workload authorizer.** `PeerAuthorizer.authorize` folded every session-registry error into
   `ErrWorkloadUnauthorized`. During a database restart, output ingest and every control RPC therefore answered the
   Worker AUTHENTICATION_FAILED (not retryable, and unlogged), and the Worker gave a live turn up (found by browser
   step 1).

**Not ported or not done here:**
- No boot-time dependency wait (B1). Boot stays fail-fast with a readable reason, and the restart policy retries it.
- The other runners' per-cycle ERROR lines during an outage are unchanged; they are an FU-299 test case (N2).
- Liveness still never detects a hung Main: FU-301.
- The Worker still gives up a live turn on a retryable output or renewal failure: FU-302 (see "Recovery guarantee").

## Changed paths

**Runner set and housekeeping runner**

| Path | What |
|---|---|
| `services/elitea-main/internal/infra/depfailure/depfailure.go:28-121` | `Transient(err, owner)` is the closed list of failures that heal on their own. **Kept:** <ul><li>SQLSTATE class 08, except `08P01`;</li><li>`08P01` only with PgBouncer's healing messages (`:40`: login retry, cannot connect to server, server conn crashed, `query_wait_timeout`, shutting down, starting up, recovery mode);</li><li>`57P01`, `57P02`, `57P03`, `53300`;</li><li>refused, reset or unresolved connections, and a broken pooled connection;</li><li>NATS `ErrTimeout`, `ErrNoResponders`, `ErrReconnectBufExceeded`;</li><li>a deadline, only while the owner context is alive.</li></ul> **Excluded (C1):** <ul><li>a closed pgx pool;</li><li>NATS `ErrConnectionClosed`, `ErrNoServers` and `ErrConnectionReconnecting` (each happens only after an explicit Close, a drain or giving up; with `MaxReconnects(-1)` never on a live, reconnecting connection);</li><li>PgBouncer's "no such database" and "no such user", and PostgreSQL protocol violations;</li><li>a wrong password or missing database, including inside a connect error;</li><li>serialization failures.</li></ul> |
| `services/elitea-main/internal/infra/depfailure/depfailure.go:123` | `Describe(err)` gives one readable cause: the SQLSTATE and server message, or the kind of network failure. It never includes the user, database, host, port or SQL. |
| `services/elitea-main/internal/runtimecomposition/publishers.go:17-26` | Restart bounds: `runnerRestartMinDelay` 1 s, `runnerRestartMaxDelay` 30 s, and `runnerCleanRunDuration` 1 min (the "clean minute", C2). |
| `services/elitea-main/internal/runtimecomposition/publishers.go:83` | `newPublisherSet` flattens nested sets and keeps each runner's name. `named(...)` labels all 21 runners at their composition sites (`composition.go`). |
| `services/elitea-main/internal/runtimecomposition/publishers.go:186-217` | `supervise` (C2): <ul><li>Restarts a runner only after its `Run` returned, so there is never a second copy.</li><li>Gives each attempt its own context, cancelled when that `Run` returns (`:190`).</li><li>Restarts only on `depfailure.Transient` (`:200`).</li><li>Keeps the failure count across restarts and resets it only after a clean minute (`:204`).</li><li>Waits on the owner context.</li><li>Logs one WARN per restart: "Main restarts the &lt;runner&gt; runner after a dependency failure, in &lt;delay&gt;", with the runner, attempt, delay and cause.</li><li>Any other stop returns `runtime runner "<name>" stopped: <cause>` and logs "The &lt;runner&gt; runner stopped and Main is exiting: &lt;cause&gt;" (`:252`). New log lines carry the described cause, never the raw error.</li></ul> |
| `services/elitea-main/internal/runtimecomposition/publishers.go:117` | `DegradedRunners()`: the runners restarted within the last clean minute. |
| `services/elitea-main/internal/runtimecomposition/toolkit_discovery_runtime.go:88-115` | Housekeeping logs each failed pass ("…pass failed; retrying in &lt;delay&gt;"). It backs off from 1 s to 30 s on a transient failure and returns to the 10 s interval after a clean pass. It **returns** any other fault (`:96`: a missing table or a closed pool), so the set names it and ends Main instead of retrying it for ever. |
| `services/elitea-main/internal/runtimecomposition/runtime.go:63-79` | `Runtime.DegradedRunners()` and `Runtime.RuntimeNATSStatus()` (client-side state, no round trip). |

**Readiness**

| Path | What |
|---|---|
| `services/elitea-main/internal/api/health/readiness.go:24-190` | `Check`, `Evaluate` and `Monitor`. <ul><li>`Evaluate` (`:59`) runs every check concurrently under one 2 s bound (`:24`), which is below the Helm (3 s) and compose (5 s) probe timeouts.</li><li>A failing critical check, or any check reporting `unavailable`, answers 503 `not_ready` (`:90`). Other failures answer 200 `degraded`.</li><li>The body holds states only, never error text.</li><li>`Monitor.Observe` (`:146`) logs one WARN per lost dependency and one INFO with the outage length when it is back (C4).</li><li>An evaluation that started before the newest applied one is ignored (`:150`).</li><li>A probe whose own context ended is not observed (`:180`), and neither is the monitor's evaluation during shutdown (`:126`).</li></ul> |
| `services/elitea-main/internal/api/health/handler.go` | `Deps.Checks` and `Deps.Monitor`. `Deps.DB` alone keeps the old single-database readiness. |
| `services/elitea-main/cmd/elitea-main/readiness_checks.go:19-104` | Main's checks. <ul><li>`db` (critical) pings the **main pool only**, so a saturated runtime admission pool cannot fail every replica at once.</li><li>`nats` reads `Conn.Status()` of the runtime and live-update connections: reconnecting is `reconnecting` (degraded); **CLOSED** is `unavailable` (503, `:98`).</li><li>`runners` reports `DegradedRunners()` and logs nothing itself.</li></ul> |
| `services/elitea-main/cmd/elitea-main/main.go:2990-3009` | The monitor runs every 5 s on a context cancelled when `run` returns, before the deferred pool closes. The router's readiness uses the same checks. `poolChecker` is removed. |

**Auth path: Worker answers during an outage**

| Path | What |
|---|---|
| `services/elitea-main/internal/transport/workloadauth/authorizer.go:30-138` | `authorize` still refuses: no identity is returned. Only a refusal caused by `depfailure.Transient` from the session registry is marked `ErrSessionStoreUnavailable` (`:100`). The marker wraps `ErrWorkloadUnauthorized` and carries no database text, session id or token. `WithLogger` (`:65`) names the server, and `observe` (`:113`) logs one WARN per outage per server and one INFO on recovery. |
| `services/elitea-main/internal/transport/runtimegrpc/control/workload_refusal.go:17-32`, `services/elitea-main/internal/transport/runtimegrpc/workspace/workload_refusal.go:10` | `workloadRefusal` answers retryable `DEPENDENCY_UNAVAILABLE` only for the marker, and AUTHENTICATION_FAILED (not retryable) otherwise. `workloadRefusalStatus` answers gRPC `Unavailable` or `Unauthenticated`. |
| **The 18 callers.** <ul><li>Output ingest: `services/elitea-main/internal/transport/runtimegrpc/output/server.go:213`.</li><li>Control: `control/server.go:225` (ClaimCommand), `:419` (BeginExecution), `:444` (AuthorizeInvocation), `:472` (RenewLease), `:510` (ObserveDesiredState), `:545` (PrepareSettlement).</li><li>Control, other files: `control/delegation_assertion.go:41`, `control/remove_index.go:50`, `control/model_checkpoint_authority.go:15`, `control/browser_session.go:93`, `control/steering.go:40`, `control/run_steps.go:50`, `control/ephemeral_agents.go:98`, `control/egress_summary.go:56`.</li><li>gRPC-status RPCs: `control/sandbox_grant.go:68`, `control/compiled_snapshot.go:46`.</li><li>Workspace claim: `workspace/server.go:96`.</li></ul> (`control/`, `workspace/` and `output/` are under `services/elitea-main/internal/transport/runtimegrpc/`.) | Every caller answers through the shared rule. None was left out. |
| `services/elitea-main/internal/transport/runtimegrpc/output/server.go:1447`, `control/server.go:879` | `safeOutputError` and `safeRuntimeError` answer a transient database error from the effect itself (ingest, claim, renew, settle) as retryable `DEPENDENCY_UNAVAILABLE`, where before it was `INTERNAL`. |
| `services/elitea-main/internal/runtimecomposition/composition.go:1485,1742` | The control and output authorizers get their logger and server name. |

**Compose**

| Path | What |
|---|---|
| `deploy/docker-compose.standalone-full.yml`, `deploy/docker-compose.yml`, `deploy/docker-compose.e2e-standalone.yml`, `deploy/docker-compose.storage-test.yml`, `deploy/docker-compose.secretstore.yml` | `restart: unless-stopped` on every long-running service that lacked one: Main, Web, postgres, pgbouncer, nats, nats-health, rustfs, oidc-mock, otel-collector, the subapp hosts, qdrant, vector, openbao, azurite and fake-gcs-server. `restart: "no"` on `nats-bootstrap`, the only one-shot without one. The Worker, the sandbox supervisor and the other daemons already had `unless-stopped`. |
| `services/elitea-main/tests/deployedge/compose_restart_policy_test.go` | The compose gate (C5). |

## Tests

| Test | Proves | Red on main? |
|---|---|---|
| `internal/infra/depfailure/depfailure_test.go` `TestTransientKeepsOnlyFailuresThatHealOnTheirOwn` | Every kept and every excluded member (C1), including a real refused pgx connect, a real closed pgx pool, PgBouncer's permanent `08P01` answers, and a deadline on both sides of the owner's state. | New package. |
| `…/depfailure_test.go` `TestDescribeKeepsTheCauseAndDropsConnectionDetails` | No user, password, host, port or database name in any description, including a real `ConnectError`. | New. |
| `internal/runtimecomposition/publishers_supervision_test.go` | <ul><li>A restart does not cancel the siblings.</li><li>At most one copy runs over 51 runs.</li><li>Delays are 1, 2, 4, 8, 16, 30, 30, 30 s for a runner failing right after each restart, and fall back to 1 s only after a clean minute.</li><li>Bounds hold over 64 failures, including with an out-of-contract jitter.</li><li>An owner cancel ends a 1 s restart wait at once.</li><li>A permanent stop names the runner and cause (non-transient error, closed pool, nil).</li><li>A runner's own timeout is a restart, not "stopped without an owning cancellation".</li><li>A failed attempt's context ends at its restart.</li><li>Nested sets are flattened and named.</li></ul> | Yes: on main a runner's own `DeadlineExceeded` ended the set anonymously, and any return ended Main. |
| `internal/runtimecomposition/toolkit_discovery_runtime_test.go` | Three PgBouncer `08P01` failures give three readable lines and waits of 1, 2 and 4 s, then the normal interval. A permanent fault (`42P01`) is returned at once, not retried. An owner cancel stops the runner. | Yes: on main Run returned the first error. |
| `internal/runtimecomposition/dependency_recovery_postgres_integration_test.go` `TestRuntimeRunnersSurviveADatabaseOutageAndLogEachTransitionOnce` | **Real PostgreSQL** behind an in-test TCP proxy, running the real toolkit discovery SQL on migrated shared schemas. <ul><li>During a cut (refused dials, reset connections) housekeeping backs off, a runner that returns its first database error is restarted, readiness is `not_ready`, and the set never stops.</li><li>Both resume after the restore.</li><li>Exactly one "Main lost its database" WARN and one recovery INFO.</li><li>No log line carries the database name, the proxy address or the server address.</li></ul> | — |
| `…` `TestRuntimeRunnersWaitOutADatabaseInRecoveryMode` | A `pgproto3` server answers every login with `57P03`. The runner is restarted with that cause, is reported degraded, and readiness is `not_ready`. Runs with no environment set. | — |
| `…` `TestWorkloadSessionRefusalsAgainstARealDatabaseAndACutOne` | **Real session registry** (auth path, condition 2). <ul><li>With the database up: a live session is authorized; unknown, expired, revoked and foreign sessions are plain, non-retryable refusals.</li><li>With the proxy cut: the live session is refused, and marked retryable five times.</li><li>One WARN and one INFO.</li><li>No session id, producer, database name or address appears in the log.</li></ul> | — |
| `internal/transport/workloadauth/session_store_outage_test.go` | Auth path, conditions 1, 2 and 4. <ul><li>Seven cases all refuse, with no identity returned and `ErrWorkloadUnauthorized` kept.</li><li>Only the `57P01` and `08P01` login-retry cases are marked. Unknown, expired or revoked sessions, a missing table, a wrong database password, a foreign workload and an unverified peer are not.</li><li>No refusal text carries the session, producer or database text.</li><li>Twenty-five requests during one outage log one WARN and one INFO, without session details.</li></ul> | New. |
| `internal/transport/runtimegrpc/control/workload_refusal_test.go` | Auth path, condition 3. <ul><li>ClaimCommand, BeginExecution, AuthorizeInvocation, RenewLease, ObserveDesiredState and PrepareSettlement each answer the marker with retryable `DEPENDENCY_UNAVAILABLE`, and a plain refusal with non-retryable `AUTHENTICATION_FAILED`.</li><li>The only call made is the authorization, so no work is done after a refusal.</li><li>No session id or SQLSTATE appears in the message.</li><li>The status RPCs answer `Unavailable` or `Unauthenticated`.</li><li>A **gate** parses `runtimegrpc` and fails if any function that calls `AuthorizeWorkload` or `AuthorizeOutput` does not classify its refusal. It found all 18 callers, and was mutation-checked: reverting `steering.go` fails it.</li></ul> | Yes: on main every refusal was `AUTHENTICATION_FAILED`. |
| `internal/transport/runtimegrpc/output/dependency_outage_test.go` | <ul><li>Output ingest answers the marker with retryable `DEPENDENCY_UNAVAILABLE`, bound to the frame with no credit, no commit and an unspecified desired state. That is exactly what the Worker accepts as "resend" (`services/elitea-worker-rust/src/transport/output_session.rs` `validate_rejection`).</li><li>Nothing is ingested.</li><li>A plain refusal stays `AUTHENTICATION_FAILED`.</li><li>`safeOutputError` maps `08P01` login retry and `57P03` to retryable, and a programming error and `28P01` to `INTERNAL`.</li></ul> | Yes. |
| `internal/transport/runtimegrpc/control/dependency_outage_test.go` | `safeRuntimeError` maps `57P01` to retryable and a programming error to `INTERNAL`. | Yes. |
| `internal/api/health/readiness_test.go` | <ul><li>Readiness table: ready; database down gives 503; NATS reconnecting gives 200 `degraded`; a runner in backoff gives `degraded`; both down gives 503; an empty probe counts as unavailable; any check reporting `unavailable` (a closed NATS connection) gives 503.</li><li>No body carries error text.</li><li>Concurrent and bounded: 300 ms checks answer in about 300 ms, and a hung check answers at the 2 s bound.</li><li>The monitor logs one loss and one recovery per dependency over 20 failing evaluations, ignores a stale evaluation that finishes last, and does not log a cancelled probe.</li></ul> | New. |
| `cmd/elitea-main/readiness_checks_test.go` | <ul><li>**Real nats-server**, killed and started again under Main's reconnect options: 200 `degraded`, then `ready`; one loss line and one recovery line. Needs `ELITEA_TEST_NATS_SERVER_BIN`, which CI sets.</li><li>A CLOSED connection gives 503 `unavailable`.</li><li>An unreachable pool gives 503.</li><li>Degraded runners give 200 `degraded` and no log line.</li></ul> | Yes: on main readiness had no NATS check. |
| `tests/deployedge/compose_restart_policy_test.go` `TestEveryComposeServiceHasTheRestartPolicyItsLifetimeNeeds` | Reads every compose file under `deploy/`. <ul><li>A service another one waits on with `service_completed_successfully` is a one-shot and must say `"no"`.</li><li>Every other declared service must say `unless-stopped`.</li><li>An overlay may inherit only from a base file (one that declares the database) that sets it.</li><li>Floors exist for Main, Web, Worker, sandbox, postgres, pgbouncer, nats, migrate and nats-bootstrap, and the test fails below 30 services.</li></ul> | Yes: 11 long-running services in the standalone stack had no policy. |

Existing tests were not edited. The local run against `postgres:18` as role `postgres` (as in CI) and a `nats-server`
v2.12.0 binary is listed under "Test run".

## Test run

Local run, at head `9f5b9f2fc`.
- **Setup:**
  - `postgres:18` as role `postgres`, the way CI runs it;
  - `ELITEA_TEST_NATS_SERVER_BIN` set to a nats-server v2.12.0, the compose version;
  - `-race`.
- **Packages:** `internal/runtimecomposition/...`, `internal/api/health/...`, `internal/infra/depfailure/...`,
  `cmd/elitea-main/...`, `tests/deployedge/...`, `internal/api` and `internal/transport/...`.
- **Result:** 3184 passed, 0 failed, 16 skipped.
  - All 16 skips are secured-NATS permission tests. They need CI's rendered chart config, the nats CLI and
    `ELITEA_TEST_NATS_URL`.
  - None of the skipped tests is new.
- **Static checks:**
  - `go vet ./...` is clean.
  - `govulncheck ./...` reports only the pre-existing GO-2026-6809 (standard library). Nothing is new.

## Performance

- **Healthy path.** A request does no new work.
  - The readiness monitor runs every 5 s: one `Ping` of the main pool and two `Conn.Status()` reads.
  - A probe adds the same.
  - Both are bounded by 2 s and run concurrently; three 300 ms checks measured at about 300 ms.
  - The authorizer's outage tracking is one mutex and one comparison per authorization.
- **Outage.** A failing runner retries at most every 30 s after the backoff ramp (1, 2, 4, 8, 16, 30 s), not every
  cycle, and a flapping runner cannot restart faster than its backoff (C2 test). Pings go through the pool's own
  connections. The outbox republished the NATS-outage admission about 60 s after NATS returned (its own backoff, up
  to 30 s plus jitter, stretched over a 60 s outage).

## Durability

- Main holds no run state in memory that an outage could lose. Claims, leases, outbox rows and receipts stay in
  PostgreSQL. The backoff only delays work that is already durable.
- A restarted runner starts from its durable state, as a process restart would. Only one copy runs, and each
  attempt's context ends with it (C2 tests).
- A refusal marked retryable has done nothing: the authorization is the first step, and the control test asserts that
  no other call follows it.

## Resilience

- Everything is bounded:
  - restart delay at most 30 s (`publishers.go:226`);
  - readiness at most 2 s;
  - monitor interval 5 s;
  - clean-minute reset 1 min;
  - the classifier is closed (`depfailure.go:28-46`);
  - the Worker's retry of a retryable answer is bounded by its own per-frame budget (`max_output_sessions` at most 8,
    `services/elitea-worker-rust/src/execution/output_delivery.rs:52`) and the 30 s claim lease
    (`services/elitea-main/internal/runtimecomposition/composition.go:95`). This change adds no Worker retry loop (auth
    condition 5).
- Typed and readable:
  - Every transition and restart is one sentence with the runner, server or dependency and its cause.
  - A fatal stop names the runner (`publishers.go:252`).
  - `runtime runner "<name>" stopped: <cause>` replaces "publisher 2" and "stopped without an owning cancellation".
- No silent coercion: a permanent failure still ends Main, by name, and compose restarts it. Examples: a closed pool,
  a closed NATS connection, a wrong password, a missing database or table, a programming error.
- **Compose stays `healthy` while `degraded`.**
  - The healthcheck runs `/elitea-main -healthcheck` against `/readyz`, which answers 200 for `degraded`.
  - An operator sees `degraded` in the `/readyz` body (`checks.nats: "reconnecting"`, `checks.runners: "degraded"`).
    It also shows in the WARN "Main lost NATS; admitted runs wait in the outbox and claimed runs continue" (C3).
  - A database outage answers 503, so compose marks Main `unhealthy` until it is back, without restarting it.
- Helm: readiness takes a replica out of the Service only for a database outage or a closed NATS connection. Liveness
  is unchanged and dependency-free (FU-301 for the hung case).

## Security

- `rules/security.md` categories:
  - **Identity and authorization (applies: the auth path).**
    - Fail-closed is unchanged. `authorize` returns no identity on any error, and every marked refusal still matches
      `ErrWorkloadUnauthorized`.
    - Only a `depfailure.Transient` cause from the session registry is marked. An unknown, expired, revoked or foreign
      session, an unverified peer and a permanent fault are refused exactly as before (table test and real cut-proxy
      test).
    - The classification is the only change. The gate test keeps every authorizing RPC on that rule.
  - **Error and log leakage (applies).**
    - The readiness body and the RPC refusals carry fixed text only.
    - Log causes go through `depfailure.Describe`, which keeps the SQLSTATE and server message and drops the user,
      database, host and port.
    - The authorizer's outage lines carry no session id, producer or workload identity.
    - The real-PG tests assert all of this over the whole captured log.
  - **Injection, URL building, egress, supply chain:** not applicable. No query, URL, outbound call or dependency is
    added; `/readyz` stays loopback-only at the browser edge (`webedge.go:76`).
  - **Secrets:** none added. The diff was scanned before every commit.
  - **Availability:** the change removes a single-fault outage. The restart policy cannot loop a failing migration,
    because one-shots keep `"no"` (compose gate).
- `/security-review` on the branch: no findings (see "Reviews").

## Recovery guarantee

| Component × phase | Class | Enforced by | Proved by |
|---|---|---|---|
| Main × PostgreSQL outage, Main process | R | Runners back off or are restarted (`publishers.go:186-217`); readiness 503 (`readiness.go:59`) | Real-PG proxy and recovery-mode tests; browser steps 1 and N3: Main `RestartCount` 0 |
| Main × PostgreSQL outage, admission | F then I | One admission transaction (`repos/agent_execution_jobs.go:107-140`); a failure writes nothing | Browser N3: "The agent run could not start (HTTP 503)", no row; the retry after recovery gave one execution with one claim |
| Main × PostgreSQL outage, claimed run (model call in flight) | **R** (in-flight model call repeated from its checkpoint; FU-302 removes the repeat) | Main answers the Worker retryable on both planes (output `server.go:213,1447`, control `workload_refusal.go:17`, `server.go:879`). The Worker still gives the turn up, and recovery resumes it from the model checkpoint (the existing Main × P02/P03/P10 path). | Browser, stack `fu275d`: claim 1 `LEASE_EXPIRED`, then claim 2 `AGENT_MODEL_CHECKPOINT`, `SETTLED`; one answer after reload. Worker log: `agent_progress.output_unavailable retryable=true`, `claim_lease_control_failure retryable=true`. |
| Main × PostgreSQL outage, outbox, output, settlement | I | Outbox backoff and exact-bytes republish (`execution/outbox_publisher.go:100-123`, `commandbus/jetstream.go:269`); retryable answers (`safeOutputError`, `safeRuntimeError`) | Real-PG proxy test; output and control mapping tests |
| Main × NATS outage, admission | I | Outbox; readiness `degraded` | Browser step 3: admitted during a 60 s stop, delivered once afterwards (one claim, `SETTLED`) |
| Main × NATS outage, claimed run | R | Lease renewal over gRPC; reconnect for ever | Browser step 2: run across `docker restart nats`, one claim, `SETTLED`, no recovery |
| Main × process crash (compose) | R | `restart: unless-stopped` | Browser step 4: Main crashed (exit by `SIGQUIT`) and was restarted by compose (`RestartCount` 1, healthy in 7 s); the session and the chat survived; a new run succeeded |

The platform matrix (`services/elitea-worker-rust/docs/recovery-guarantees.md` sections 1, 5, 8 and 9) carries these
rows. The old row "Main x PostgreSQL and NATS crash: pools reconnect" was not true on compose and is replaced.

## Browser

All steps ran in Chrome against disposable stacks, with no response mocked. Every stack was restored from
`product-real-models-main-ce2756fad.dump` with the pre-defaults tool and the checkout's default compose:
`verify-profile` 50 passed, 0 failed, every time. Steps were taken as `admin@centry.user` in project 2, model
`gpt-5.4-mini`. All stacks are torn down.

**Images**
- Main `fu275-9f5b9f2fc` (`sha256:59223d77…`): binary-check marker "cannot read workload sessions" present.
- Worker `main-ce2756fad-verify` (`sha256:79ec4d45…`), built from main.
  - The older `fu287` Worker rejected main's input as not canonical, so a Worker built from main was required.
- Web `main-ce2756fad-verify`.
- Supervisor `pm-778e5beb8`.
- Earlier stack `fu275c`: Main `fu275-858a5555b`.

| Step | Fault and action | Seen |
|---|---|---|
| 1 (`fu275c`, before the auth fix) | `docker restart postgres` while an essay streams | <ul><li>Main kept running (`RestartCount` 0, healthy).</li><li>`/readyz` answered 503 `{"db":"unavailable"}` during the outage and 200 `ready` after.</li><li>Logs: "Main lost its database …", then "Main reached its database again after 15s".</li><li>The Worker got `output_rejected retryable=false` (AUTHENTICATION_FAILED from the authorizer); claim 1 `LEASE_EXPIRED`, claim 2 `AGENT_MODEL_CHECKPOINT`.</li><li>One answer after reload.</li><li>This finding led to the auth-path change.</li></ul> |
| N3 (`fu275c`) | `docker stop postgres`, then send a message | <ul><li>The UI showed "The agent run could not start (HTTP 503)".</li><li>No row was written, and the message was not kept after reload.</li><li>Main never restarted.</li><li>PgBouncer served its cached "server DNS lookup failed" for about 30 s after the database was back, so "Main reached its database again after 45s".</li><li>The session survived (no new sign-in).</li><li>The message sent again after recovery gave **one execution, one claim, `SETTLED`**.</li></ul> |
| 2 (`fu275c`) | `docker restart nats` while an essay streams | <ul><li>Both NATS connections reconnected within 1 s. The outage was shorter than the 5 s monitor interval, so no transition line.</li><li>One claim, `SETTLED`, with no recovery.</li></ul> |
| 1 rerun (`fu275d`, with the auth fix) | `docker restart postgres` while a turn is in flight | <ul><li>Main kept running.</li><li>Logs: "The runtime output server cannot read workload sessions; Workers are answered "retry" …" once; "Main lost its database" once; "…control server cannot read workload sessions" once; "Main reached its database again after 15s"; then each server's recovery INFO.</li><li>Worker: `agent_progress.output_unavailable retryable=true` and `claim_lease_control_failure retryable=true`. The Worker then retained the delivery, claim 1 `LEASE_EXPIRED`, claim 2 `AGENT_MODEL_CHECKPOINT` `SETTLED` (FU-302).</li><li>One answer after reload.</li></ul> |
| 3 (`fu275d`) | `docker stop nats` for 60 s; send a message during the outage | <ul><li>`/readyz` answered 200 `{"status":"degraded","checks":{"db":"ok","nats":"reconnecting","runners":"ok"}}`, and compose kept Main `healthy`.</li><li>The message was accepted with no error, and nothing claimed it while NATS was down.</li><li>Logs: "Main lost NATS; admitted runs wait in the outbox …" once; both connections reconnected; "Main reached NATS again after 1m5s".</li><li>The admitted run was claimed once and `SETTLED`, and was answered after reload.</li></ul> |
| 4 (`fu275d`) | Main crashed (`SIGQUIT` to PID 1 from a sidecar in its PID namespace; `docker kill` counts as a manual stop, which no restart policy restarts) | <ul><li>Compose restarted it: `RestartCount` 1, running at once, healthy 7 s after the crash.</li><li>After reload the session and the chat were intact (no sign-in).</li><li>A new message was answered (one claim, `SETTLED`).</li></ul> |

## Reviews

- `/code-review high` on the branch raised ten findings, all fixed in `238b9a226`:
  1. A closed NATS connection read as "reconnecting".
  2. Housekeeping swallowed permanent faults.
  3. A cancelled probe was logged as an outage.
  4. Out-of-order evaluations could break once-per-outage logging.
  5. The admission-pool ping could fail every replica.
  6. Raw `err` attributes were logged.
  7. PgBouncer's permanent `08P01` answers counted as transient.
  8. Goroutines of a failed attempt could outlive it.
  9. The monitor could outlive `run`.
  10. Helpers were duplicated.
- `/security-review` on the branch: no findings at confidence 8 or above. The `/readyz` body and log causes were
  checked for leakage; the classifier was checked for an authorization bypass (`28P01`, a closed pool and a closed
  connection stay permanent).

## Follow-ups

- FU-302: the Worker gives a live turn up on a short Main or database outage, so the in-flight model call repeats
  from its checkpoint. Target: hold the turn within the lease over PgBouncer's 15 s window. Overlaps G-PG-01.
- FU-301: liveness never detects a hung Main (from C6).
- FU-299 test case: the other runners' per-cycle ERROR lines during a database outage (N2).
- N1: PgBouncer's `server_login_retry` stays at the 15 s default and is not tuned. Its cached DNS failure lasted about
  30 s in step N3.
