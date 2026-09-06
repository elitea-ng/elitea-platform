# DECISIONS — elitea-llm-gateway

Standing decisions for the LLM gateway, with rationale. `CLAUDE.md` is the terse
agent-facing version of these; this file is the human-readable "why". Each entry
records a decision so it is not re-litigated on every PR. Items marked **[human
decision]** are risk/policy calls an autonomous agent must NOT change without sign-off.

## Money
- **Only the TERMINAL stream event carries authoritative usage.** bifrost maps
  Anthropic's `message_start` to `response.created`, and that event already
  carries usage (input tokens, `output_tokens: 1`). Accepting usage from any
  event made a mid-stream disconnect bill "input + 1 token" as authoritative AND
  suppress the `budget.unbilled_stream` loss event — an invisible underbill
  across the whole `/llm/v1/messages` dialect, strictly worse than the visible
  loss issue #9 set out to fix. `responsesUsageFromChunk` therefore accepts
  usage only from `response.completed` / `.incomplete` / `.failed`.
- **int64 nano-USD, no float on the money path.** Rationale: float rounding drift
  across the incr → delta → Postgres write-behind hops corrupts billing; integers
  are exact. Prices are per-1M tokens (a per-1k reading is a 1000× overcharge).

## Enforcement
- **[human decision] Fail CLOSED on budget-store/PG error while NATS is up.**
  Chosen 2026-07-23 over fail-open. Rationale: this is a metered spend surface; a
  transient DB read failure returning an "unlimited" snapshot would let spend run
  unbounded. We accept returning 503 during a DB blip over granting free spend.
  Reconsider only if availability SLOs make brief 503s unacceptable AND a safer
  bound exists.
- **[human decision] 2026-08-09 (supersedes the entry below) — the
  concurrent-admission overshoot is UNBOUNDED and accepted; the in-flight
  reservation is deleted.** *Finding (issue #10):* the reservation that the
  entry below claimed as the bound never existed in practice. All nine
  `checkBudget` call sites passed `promptTokenEst=0`, so the `+reservation`
  increment was unreachable, while the billing path decremented
  unconditionally — the counter only ever drifted negative and its `sync.Map`
  entries were never reaped (unbounded growth per project+period).
  *Decision:* delete the mechanism, its counter and its claims rather than
  repair it. Bounding the overshoot for real needs a pre-flight token
  estimator, and no estimate may reach a billed amount (the money-path rule
  below); nobody has asked for concurrent admission control. `checkBudget` now
  passes `reqCostNano=0` explicitly. *Accepted cost:* under burst, N concurrent
  requests can each be admitted against the same not-yet-updated NATS counter;
  the overshoot is bounded only by burst size × per-request cost. The NATS
  counter remains ground truth and the hard 402 still fires once it updates.
  Revisit — with a real estimator — if a project can materially exceed budget
  under burst in practice.
- **[human decision] (SUPERSEDED 2026-08-09 by the entry above — the bound it
  claims was never wired) Async-billing concurrent-admission overshoot is a
  bounded, accepted trade-off.** Billing moved off the HTTP critical path
  (latency); the window where N concurrent requests pass admission before the
  counter updates was said to be bounded by an in-flight reservation. The
  residual overshoot is accepted for the latency win.
- Every /llm endpoint is gated + billed uniformly (no per-endpoint exceptions).
- **[human decision] 2026-08-05 — Streamed spend on client disconnect: bounded
  grace, authoritative usage only.** *Finding:* streams billed only from the
  final usage chunk, so any early exit (client disconnect, mid-stream provider
  error, failed stream setup) left the whole response unbilled — a reachable
  hard-budget bypass (issue #9). *Decision:* build the streaming provider
  context with `context.WithoutCancel(r.Context())` and an owned cancel, so the
  provider stream survives a client disconnect for a bounded grace period
  (`LLM_STREAM_GRACE_MS`, default 5 s, clamped ≤15 s, 0 = disabled) during which
  a detached drain may still capture the authoritative usage trailer. If no
  trailer arrives, **nothing is billed** and a `budget.unbilled_stream` event is
  emitted on gateway.events.*. (AMENDED 2026-08-28 by the issue #79 entry
  below: a cut stream now settles from the provider's own accumulated count
  when there is one, and still publishes the event. Nothing is billed below
  that floor, so the anti-estimate half of this decision is unchanged.) *Rationale:* the free-inference lever is only
  valuable to a client that already received nearly all the output, which is
  exactly when the trailer is closest — so a short grace covers the exploitable
  window. An observed-output-bytes estimate was explicitly REJECTED: it would
  put a `bytes/4` heuristic on the money path (no estimate may ever reach a
  billed amount), over-bill inline-base64 multimodal by orders of magnitude, and
  fire on clean completions the loop cannot distinguish from disconnects.
  Out-of-band provider usage lookup was rejected as unavailable in practice
  (no per-request usage API on Anthropic/Bedrock/Vertex/Ollama/vLLM; OpenAI-only
  and minutes late). *Accepted cost:* a client that disconnects — including a
  user pressing Stop — may be billed for up to the grace period of further
  generation, charged to that project's own provider credential. Concurrent
  drains are bounded (`LLM_STREAM_DRAIN_MAX_INFLIGHT`) and shutdown cuts them
  loose so the pod's termination grace is never held hostage to the stream grace
  (see the two-phase drain entry below). The loss event is operator-only.
- **[human decision] 2026-08-05 — Drain-pool saturation fails to a SHORT grace,
  never to "bill nothing"; slots are bounded per project.** *Finding:* the first
  cut of the above dropped a saturated drain straight to grace=0, destroying a
  trailer that is usually milliseconds away — reopening the same fail-open hole
  on a single global counter any one tenant could exhaust (gateway-review).
  *Decision:* a drain that cannot take a slot gets `SaturatedStreamGrace`
  (500 ms) instead of zero, and the pool is bounded per project
  (limit/8, floor 1) as well as globally. The loss event reports
  `drain_saturated` — and `drain_saturated/<what actually happened>` when the
  short grace was still not enough — so saturation is alarmable before it costs
  money. *Rationale:* the resource being protected is a provider socket that is
  about to close anyway; refusing new streaming admissions instead (fail closed
  at the gate) turns a billing edge case into an availability event, which is
  the worse trade at this severity.
- **[human decision] 2026-08-05 — Shutdown is a THREE-phase sequence over a
  split server lifecycle.** *Finding (round 2):* the two-phase version below was
  still wrong in sequence. `srv.Shutdown` closed NATS as its last step, so
  moving the billing drain after it sent every increment to a dead connection
  (diverted to the outage-delta path, which recovery only sweeps on a breaker
  CLOSED transition that never fires while NATS is healthy); the `os.Exit(1)` on
  a shutdown error then sat between the two, skipping the drains exactly when
  they matter; and drains spawned after phase 1 were left off the wait group, so
  every drain on the shutdown path was skipped (reproduced: 0 increments, 5/5).
  *Decision:* split `Server.Shutdown` into `ShutdownHTTP` (HTTP + core, NATS
  stays UP) and `Close` (NATS), and run
  `ShutdownHTTP → StopStreamGrace → DrainBilling → govStore.Drain → Close`.
  Billing is open and NATS is live throughout the window in which streams
  actually settle. A failed HTTP shutdown no longer aborts the drains.
  `terminationGracePeriodSeconds` is raised to 180 so the post-HTTP phases have
  headroom over the 150 s drain budget.
  **StopStreamGrace MUST NOT be hoisted above ShutdownHTTP** (round 3): it both
  sets `drainsClosing` and closes the `drainClosing` channel, so running it
  first gives every stream disconnecting during the ~150 s HTTP drain `grace=0`
  and cuts every parked drain — turning disconnect billing OFF for the whole
  duration of every rolling deploy, i.e. reopening the issue-#9 bypass. That
  regression shipped once and was invisible to a test whose SSE loop observed
  the usage chunk before the failing write, so the drain never participated.
  Drains are detached goroutines, not HTTP requests, so leaving the grace armed
  does not extend `ShutdownHTTP` at all. `TestShutdownSequence` asserts the
  ORDER BY EXECUTING it — the previous textual guard compared source positions
  and could not see the `os.Exit` between two calls.
- **[human decision] 2026-08-05 — (superseded by the entry above) Shutdown is a
  two-phase drain, and a refused billing increment is metered.** *Finding:* `drainForShutdown` ran BEFORE
  `srv.Shutdown`, so `billingClosing` was set while SSE handlers were still
  live; a drain that had recovered the authoritative trailer had its increment
  refused and — because usage WAS known — no loss event was emitted either.
  Reproduced on the deploy path: 231000 nano-USD lost, 0 UpdateUsage calls, 0
  events. *Decision:* split shutdown into `StopStreamGrace()` (phase 1, before
  `srv.Shutdown`: drains stop waiting for trailers, billing stays open) and
  `DrainBilling()` (phase 2, after: waits for drains on their own WaitGroup,
  THEN closes billing and waits for billing goroutines). Independently, when an
  increment is refused with usage in hand, publish `budget.unbilled_stream` with
  reason `billing_refused`. *Rationale:* the two halves of the old
  `DrainBilling` had opposite timing requirements; and known spend must never
  disappear into a lone WARN. The metering half stands; only the ordering was
  superseded. A refused increment is reported as `billing_refused`, which is
  raised ONLY for a genuine drop — "no gate wired", "no resolvable project" and
  "zero-priced model" are `billNotBillable` and stay silent, or the one alarm
  that detects real loss drowns in noise.
- **[human decision] 2026-08-28 — A cut stream is billed from the provider's
  accumulated count, and the loss event still fires (issue #79).** *Finding:*
  when the gateway cuts a stream itself (grace expired, graceful shutdown,
  drain-pool saturation) the provider has already processed tokens it will
  charge for, and the gateway metered the whole stream as a total loss. bifrost
  maintains that count for exactly this purpose. *Decision:* settle such a
  stream from the accumulated count, under three rules — a first attempt
  (0e75501, reverted in 32b3e3c) was wrong on all three. **(1) Source it from
  the CONTEXT HANDLE** (`BifrostContextKeyStreamAccumulatedUsage`), never from
  the cancellation chunk: bifrost sends that chunk with `select { send;
  <-ctx.Done() }` against a context the gateway has ALREADY cancelled, so both
  cases are ready and Go picks at random (measured: 19 of 40 cut streams
  billed). **(2) Reject a count at or below `placeholderOutputTokens` (1).**
  Anthropic and Bedrock-Anthropic emit `output_tokens: 1` in `message_start`
  before any generation, and `accumulateAnthropicResponsesUsage` MAX-merges, so
  a mid-stream cut sees that placeholder unchanged — the very value
  `isTerminalResponsesEvent` exists to reject. An accumulated count is billable
  because it measures work done, not because it is accumulated. **(3) A partial
  bill STILL publishes `budget.unbilled_stream`**, with reason
  `partial_provider_usage` and the partial counts on the payload. The first
  attempt set `gotUsage` on the fallback, which suppressed the event and turned
  a visible, alarmable loss into an invisible ~89% underbill. Billing something
  is not the same as billing correctly. *Precedence:* terminal trailer >
  partial > nothing; the partial is consulted only when no trailer arrived, so
  a recovered trailer is never overwritten downward. *Synchronisation:* the
  handle is a pointer the provider goroutine mutates in place, so it is read
  ONLY after the provider closed the channel — every bifrost provider closes it
  from that goroutine after its last write, and that close is the sole
  happens-before edge. A settler cut off by the hard backstop reads nothing.
  *Money-path rule untouched:* these are provider-reported counts of work done,
  not a gateway-side estimate.
- **[human decision] 2026-08-05 — `budget.unbilled_stream` is operator-only.**
  It publishes to `gateway.events.ops.budget` via a separate `OpsEventPublisher`
  port, NOT the per-project subject elitea-main relays to project members.
  *Rationale:* `budget.soft_alert` is tenant-facing by design, but telling a
  tenant in real time which of their streams the gateway failed to bill is an
  oracle for the conditions that produce it.

- **[human decision] 2026-08-09 — "Circular-routing guard #2" is an
  amplification backstop, not a loop detector; its numbers are operator
  settings (issue #12, INTERIM).** The implementation does no hop detection at
  all — it counts requests per (project_id, model). At the spec'd 5/1s/30s it
  was a hardcoded 5 req/s rate limiter armed in production; a 50-VU k6 run
  against one tuple measured 99.96% HTTP 429, so the breaker, not the gateway,
  was what the overhead gate measured.
  The load-bearing finding: **no rate threshold can separate a routing loop
  from legitimate traffic here**, because both are bounded by the same
  per-replica provider worker pool. Low enough to catch the canonical loop is
  low enough to trip ordinary bursts; high enough not to trip bursts can never
  fire on the loop. So the layer is now named and tuned for what it measurably
  is.
  Numbers: threshold 1000 (`LLM_LOOP_BREAKER_THRESHOLD`), window 1 s, open 5 s
  (was 30 s). 1000 = provider worker pool (50) ÷ fastest realistic call latency
  (~50 ms embeddings) — the ceiling of what one replica could ever serve for one
  tuple. Worst case permitted: 1000 req/s per tuple per replica × replicas,
  sustained, versus the old 5 — accepted deliberately, because the old number's
  containment was illusory while its false positives were measured. A negative
  threshold disarms it; either way `logLoopBreakerMode` states the mode at
  startup, so it can never quietly pretend to be armed.
  Guarded by `TestLoopBreaker_DefaultDoesNotTripOrdinaryBurst`,
  `TestLoopBreaker_DefaultStillContainsRunawayAmplification`,
  `TestLoopBreaker_OperatorParamsApply` and the `logLoopBreakerMode(` wiring-gate
  entry; all mutation-verified 2026-08-09.
  **NOT done here, tracked as follow-up:** hop-marker detection (the actual
  mechanism), and amending spec §2.6 + `runbook-bifrost-cutover.mdx`, which
  still document 5/1s/30s and now disagree with the code — those live in
  `elitea-docs`, a different repository. Both are now DONE — see the entry
  below and EliteaAI/elitea-docs#13.

- **[human decision] 2026-08-28 — the loop detector is a HOP MARKER, and
  recognising one refuses exactly one request (issue #164).** *Mechanism:* the
  gateway stamps `X-Elitea-Llm-Hop` on every outbound provider request
  (`internal/account` → `NetworkConfig.ExtraHeaders`) and refuses any inbound
  request that already carries this deployment's own value
  (`internal/llmproxy/hopguard.go`, mounted on the router ROOT). A circular
  route is contained on its FIRST re-entry, at any rate — including the one
  request per hour that no threshold in `loopbreaker.go` can ever see. The
  refusal is `400 invalid_request_error / circular_routing_detected`.
  *Why 400 and not 508 Loop Detected:* 508 is a 5xx and the OpenAI-compatible
  SDKs on this path retry 5xx, so a request already known to be a loop would
  re-enter the platform twice more before the client gave up. No SDK retries a
  400, and the condition genuinely is a caller-side configuration fault.
  *Key material:* `GATEWAY_HOP_SECRET`, never `GATEWAY_IDENTITY_SECRET` and
  never derived from it. The marker travels to every upstream and a provider
  `api_base` is tenant-authored, so the key that signs the `X-Elitea-*`
  identity headers must not follow it there, and marker rotation must not force
  an identity rotation. Same value on every replica: a loop can leave through
  replica A and re-enter on B.
  *The security property, and why it constrains the design:* the marker cannot
  be secret — every upstream receives a copy and the browser edge forwards it
  on purpose. What holds instead is that recognising a marker refuses ONLY the
  request carrying it. Detection reads one header, compares it in constant time
  and records **nothing** — no counter, no circuit, no map. A hop refusal must
  therefore never feed the amplification backstop or any shared state: a value
  every upstream already holds would otherwise become a cross-tenant denial of
  service, i.e. the guard would create the failure it exists to contain.
  `TestHopGuard_ForgedMarkerDeniesOnlyTheForger` and
  `TestHopGuard_LoopBreakerIsNotConsulted` pin this.
  *Edge exception:* the browser edges delete the whole `X-Elitea-*` set (#326).
  This ONE name is exempt, because it is not identity — it grants nothing,
  names no project, and the only thing a client achieves by sending it is the
  refusal of its own request. `mustForwardHeaders` in
  `services/elitea-main/tests/deployedge/edge_identity_strip_test.go` enforces
  the exemption; `internal/hopmarker/path_pin_test.go` fails when the two
  modules drift, when any file under `deploy/` deletes the marker, or when
  elitea-main's `/llm` proxy names it.
  *Never silently disarmed:* `logHopMarkerMode` states ARMED or UNARMED once at
  startup, in the idiom of `logLoopBreakerMode`.
  **KNOWN GAP, deliberately left visible:** the Helm chart offers no
  `GATEWAY_HOP_SECRET` knob, so a default install boots **loudly unarmed**.
  Wiring `secrets.GATEWAY_HOP_SECRET` in `deploy/helm/elitea/values.yaml`
  beside `GATEWAY_IDENTITY_SECRET` is what arms it; the reason and the
  follow-up are recorded in `scripts/env-drift-allowlist.txt`, which is what
  keeps CI green in the meantime.
- **2026-08-28 (issue #315) — budget enforcement is INSTALLABLE at runtime, and
  the install is monotonic.** *Finding:* `server.New` dials NATS once, and
  nats.go only resurrects a connection that succeeded at least once, so a
  gateway that starts during a NATS outage enforces nothing for the life of the
  process. Issue #304 made that visible (`/readyz` reports not ready); it could
  not make it recover, because `Handler.budgetGate` was a plain field that the
  money path read with no synchronisation — installing it late was a data race
  on billing. *Decision:* publish the whole NATS budget path (gate, price
  calculator, both event publishers, the `budget_used` reader) as ONE immutable
  value behind ONE `atomic.Pointer` (`internal/llmproxy/budget_plane.go`). A
  reader takes a snapshot with `h.budget()` and uses the fields of THAT
  snapshot; a writer publishes a new value. The composition root polls
  `server.RedialNATS` and calls `Handler.InstallBudgetEnforcement`
  (`cmd/elitea-llm-gateway/budget_recovery.go`). *Rules that hold this up:* the
  install happens ONCE — `InstallBudgetEnforcement` refuses a nil gate and
  refuses to replace an installed one, so the gate is never swapped under an
  in-flight request and enforcement is never turned OFF by a transport error.
  That monotonicity is what makes per-function snapshots safe for a request that
  spans admission, billing and the soft alert. *Not done:* the authored
  per-minute rate limiter is bound at startup and stays disabled until the pod
  restarts; the recovery log line says so. Guarded by
  `TestInstallBudgetEnforcement_UnderLoad` (which measures nothing without
  `-race`), `TestEnforcementPlane_ReadyzFlipsUnderLoad`,
  `TestBudgetRecovery_InstallsWhenNATSReturns` and the `startBudgetRecovery(`
  wiring-gate entry.
- **2026-08-28 (issue #510) — a ceiling that refuses owes its owner a warning,
  so the soft alert is now per SCOPE.** *Finding:* #321 gave the gateway a
  member ceiling that answers 402 `member_budget_exceeded`, and `trySoftAlert`
  stayed project-scoped. A project crossed its threshold and an alert went out;
  a member crossed the same threshold and nothing did. The refusal was the first
  signal that a cap applied to that member. Before #321 the asymmetry cost
  nothing, because a member cap refused nothing. *Decision:* `trySoftAlert`
  takes `scope` + `scopeID` and runs for BOTH ceilings. The member gets its own
  pre-increment snapshot, read beside the project one in the billing goroutine;
  it is one more gate read for each request that bills a member, and FIX #27 has
  already moved that block off the request goroutine, thus the caller waits for
  none of it. *Rules that hold this up:* (1) the two crossing tests are
  independent conditions, not early returns — a project increment that failed
  must not silence the member's warning, because the member's counter moved;
  (2) `TryAlertCooldown` derives its key from (scope, scope_id, period), thus
  the two scopes claim different cooldowns and neither suppresses the other;
  (3) the #322 platform switch gates both, because `userSnapshotSQL` reads
  `alerts_enabled` from the same global row; (4) the event is published on the
  PROJECT's subject for both scopes — `PublishSoftAlertEvent` turns its first
  argument into `gateway.events.project.<id>.events`, and a member scope_id
  there builds a subject nothing subscribes to. The payload carries `scope`,
  `scope_id` and `user_id` instead; `project_id` stays the numeric project id on
  both scopes, so the three fields a project alert already had keep their old
  values. Guarded by `TestMemberSoftAlert_CrossingEmitsMemberScopedEvent`,
  `TestMemberSoftAlert_BelowThresholdEmitsNothing`,
  `TestMemberSoftAlert_NoMemberIDEmitsNothing` and
  `TestMemberSoftAlert_PlatformSwitchSuppressesMember`. *Not done:* spec §8.3
  still documents a project-only `budget.soft_alert` payload; it lives in
  `elitea-docs`, a different repository.

## Authored governance (issue #218, 2026-08-23)

`gateway.governance_config` is the table elitea-main's admin surface writes and
this gateway now reads. Before #218 nothing read it: `internal/governance` is
the budget COUNTER engine over `gateway.project_budget`, and a rule saved on the
admin surface was enforced nowhere. The table's own migration header and the
admin schema each claimed otherwise, and both were wrong.

`internal/policy` is the new plane. It is a SEPARATE package from
`internal/governance`, deliberately: one enforces spend against a counter, the
other enforces what an operator wrote down. They share no state and meet in one
place — `BudgetDefaults`, wired in the composition root.

**Decisions that are policy, not mechanism. Do not change these without a human.**

- **Rate limits FAIL OPEN when the NATS counter is unreachable**, and every
  fail-open is counted (`Limiter.Degraded()`, served on `/governance/status`). A
  rate limit protects against overload; it is not the spend control. Failing
  closed would turn a NATS outage into a total outage, and the budget gate — which
  IS the spend control — has its own fail-mode FSM for that decision.
- **A failed definition refresh keeps the PREVIOUS snapshot.** Dropping to the
  empty snapshot on a transient database fault would silently lift every
  allowlist and every rate limit at the moment the platform is least healthy.
  The staleness is reported instead (`Store.Status().Error` alongside
  `last_success`).
- **An unreadable credential rate policy bills NORMALLY.** `billed` is the only
  safe default: the other two suppress accounting, and a definition that failed
  to load must never be the reason spend goes unrecorded. That is the one
  failure direction with no way back.
- **A routing rule that ERRORS is skipped, never treated as matching.** An
  erroring predicate says nothing about whether the rule applies, and defaulting
  it to "apply" would let a typo in one rule re-route a whole deployment.
- **Routing runs BEFORE the model allowlist**, both inside `mapModel`. A rule
  must not be able to route a request to a model the operator forbade; the
  routed target is what the allowlist judges. `TestRoutingCannotEscapeTheModelAllowlist`
  pins the order and was mutation-verified.
- **An authored budget is a FALLBACK only.** It is consulted on the branch that
  used to return "no row ⇒ unlimited", so a project gains a ceiling it did not
  have and never loses one it did. A per-project `gateway.project_budget` row
  always wins.

**Limits that are real, and are stated in the UI and in the schema rather than
in a release note.**

- **A token ceiling is enforced on the request AFTER the one that crossed it.** A
  request's token cost is unknown until the provider answers. Doing better needs
  a tokeniser for every provider dialect on the hot path.
- **There is NO warm reload.** The design offers "at load, or when a warm-reload
  event is issued"; only the poll is implemented (`LLM_GOVERNANCE_REFRESH_SEC`,
  default 30 s). The poll converges unconditionally — a replica that missed an
  event because it was starting, or because NATS was down, still picks the
  change up — and a second path that is usually redundant is a second path that
  can be silently broken. A definition takes effect within the interval.
- **`scope.team_ids` is refused on write and makes an existing row INERT.** This
  platform has no teams: no migration creates the table and no request carries
  one. The row is reported, not silently dropped.
- **Four CEL variables are declared and unevaluable** — `team_id`,
  `tokens_used`, `complexity_tier`, `headers` — and a rule naming one is refused
  at authoring with the reason. They stay DECLARED so the authoring and
  enforcement environments remain identical; `policy.UnevaluableCELVariables` and
  elitea-main's `unevaluableCELVariables` are pinned together by
  `TestUnevaluableCELVariablesMatchTheGateway`, which reads the gateway source.
- **`GET /governance/status`** is the operator's answer to "is the rule I saved
  in force?". It reports the snapshot's `loaded_at`, every row REJECTED with its
  reason, and every row that loaded but can match nothing. The admin SPA cannot
  reach it: the admin surface does not talk to the gateway (design §5), and a
  proxy for it is a separate decision.

## Trust boundary
- **[human decision] Deny-by-default trusted-proxy model.** `X-Auth-*` identity
  headers are honored only from `TRUSTED_PROXY_CIDRS` (matched on RemoteAddr, not
  the spoofable X-Forwarded-For); empty config = trust nothing. The edge strips
  all client-supplied auth material before proxying. Rationale: without the CIDR
  gate, any pod with network reach could assert an arbitrary project identity.
  Operators MUST set TRUSTED_PROXY_CIDRS to their ingress range.
- **[human decision] 2026-08-09 — `GATEWAY_IDENTITY_SECRET` is REQUIRED, and a
  default install without it does NOT boot (issue #11).** The old startup guard
  fired only when `GATEWAY_NATS_URL` was set, but the vault-backed Account is
  wired on `pool != nil`. `verifySignature` treats an empty secret as
  "verification disabled" and returns true, so a NATS-less deployment with a
  database took `X-Elitea-Project-Id` at face value and used *that* project's
  decrypted Fernet-vault provider credentials — unauthenticated cross-tenant
  credential use. The guard now also covers the Account path
  (`startupIdentityCheck`, `cmd/elitea-llm-gateway/main.go`), FATAL + exit(1)
  before `server.New` creates a listener.
  **Consequence, chosen deliberately over the alternatives:** `DATABASE_URL` has
  a non-empty default and `pgxpool.New` only parses the DSN (it never dials), so
  `credentialAccount` is true in effectively every deployment — the secret is now
  unconditionally required in practice. The chart therefore flips
  `secrets.GATEWAY_IDENTITY_SECRET.optional` to `false` in BOTH
  `deploy/helm/elitea-llm-gateway` and `deploy/helm/elitea-main` (the two sides
  must carry the same value; the edge signs what the gateway verifies). A
  `helm install` with no `identity-secret` key now fails at container creation
  with a message naming the Secret, instead of silently starting a gateway that
  hands any caller any tenant's credentials. Dev/compose/CI runs must set
  `GATEWAY_IDENTITY_SECRET` to any non-empty value.
  Rejected alternatives: (a) keep booting and merely drop the credential-backed
  Account — a silent, hard-to-diagnose loss of all provider calls that still
  leaves budget identity unverified; (b) keep `optional: true` and let the
  binary crash-loop — same outcome, worse diagnostics.
  Guarded by `TestStartupIdentityCheck` + the `startupIdentityCheck(` entry in
  `TestMainWiring`; both mutation-verified 2026-08-09.

- **[human decision] 2026-08-09 — Tenant-authored `api_base` may reach a private
  network ONLY through an operator egress allowlist (issue #13).** The previous
  `AllowPrivateNetwork = true` carve-out for the vLLM/Ollama classes was
  unconditional, and the URL those classes dial comes from the tenant's own
  `p_{id}.configuration` row — so any user who could author a credential could
  make the gateway open a connection to any address the pod can reach. Two
  modes now, and deliberately no third:
  - `GATEWAY_EGRESS_ALLOWLIST` empty (default): hosts unrestricted, but
    bifrost's SSRF-safe dialer stays armed for EVERY provider class. No tenant
    can reach RFC-1918. **Self-hosted vLLM/Ollama on a private network stops
    working until an operator opts in** — that is the accepted cost.
  - non-empty: every `api_base` must match an entry, and private destinations
    become reachable for the self-hosted classes only.
  It is a HOST-NAME allowlist, not an IP-range check, on purpose: an IP check
  here would be a check-then-dial race (DNS rebinding), whereas a name the
  operator sanctioned is sanctioned whatever it resolves to, and the dialer's
  own check happens at connect time with no race. The check runs BEFORE
  `vault.Resolve`, so a non-allowlisted destination never causes a decrypt of
  the tenant's `{{secret.NAME}}` key material.
- **2026-09-05 — The connection probe uses the DIALER's egress predicate, not a
  second one.** The egress-governance work (gap G6) split one question into two:
  `EgressAllowlistConfigured` answers "is an allowlist armed", and
  `allowsPrivateNetwork` answers "does an entry EXPLICITLY name a private host
  or CIDR". The dialer moved to the second question; `/llm/v1/check_connection`
  and `/llm/v1/list_provider_models` kept the first. With a name-only entry —
  the chart's own example — the two disagree. "Test connection" then reported
  success and wrote `status_ok`, and every chat turn died in the dialer.
  `EgressPolicy` now declares `EgressPrivateNetworkAllowed`, and
  `probeAllowsPrivateNetwork` (internal/llmproxy/checkconnection.go) is the ONE
  place both routes read it. Read it from the account's gate, never from the
  environment: the gate merges the authored `egress_allowlist` governance rows
  with the `GATEWAY_EGRESS_ALLOWLIST` floor. Guarded by
  `TestProbeEgressPredicate_MatchesTheDialer`, which drives the real account and
  compares the probe's decision with `GetConfigForProvider`; mutation-verified
  2026-09-05.
- **[human decision] 2026-08-09 — Upstream response bodies are NEVER echoed to
  callers (issue #13).** bifrost/core puts an unparsable non-2xx body verbatim
  into `Error.Message` (`core@v1.7.3 providers/utils/utils.go`, prefix
  `"provider API error: "`), and the gateway copied it into the client-visible
  envelope — turning a blind SSRF into a full read of anything the pod can
  reach. `sanitiseUpstreamMessage` (internal/llmproxy/httpio.go) replaces that
  form with a status-only message and caps every other upstream message at 256
  bytes as a second line of defence. Messages bifrost PARSED out of a structured
  provider error (quota text, rate-limit detail) are preserved — those are the
  tenant's own useful diagnostics.
  Guarded by `TestOpenAIErrorBody_DoesNotEchoUpstreamBody`,
  `TestGetConfigForProvider_PrivateNetworkGatedOnAllowlist` and
  `TestGetKeysForProvider_EgressGuardBeforeVault`; all mutation-verified
  2026-08-09.
  **Residual, NOT fixed here:** with no allowlist configured, a tenant can still
  ship its own vault-resolved secret to an arbitrary PUBLIC host as a Bearer
  token (`api_base: https://attacker.example/v1`). Setting the allowlist closes
  it; making that mandatory would break every cloud-provider install and is a
  separate human call. The chart's opt-in `networkPolicy` is defence in depth,
  not the primary control — see values.yaml for why it cannot default to on.

## Shared/public project scope
- **[human decision] The public project id is OPERATOR CONFIGURATION
  (`ELITEA_AI_PROJECT_ID`), not a field on the signed identity.** Chosen
  2026-08-14 for issue #316, which allowed either shape and asked that the
  choice be recorded.

  *Context:* the gateway read `p_{caller}` only. Elitea has always had two
  sources of models for a project — its own rows, and the public project's
  `shared = true` rows. The UI pickers offer both (`include_shared`), so a user
  could select a platform model that the gateway then had no credential for. In
  the `ELITEA_ALLOW_PROJECT_OWN_LLMS=false` deployment mode that left no usable
  model at all.

  *Rationale for configuration over the identity header:*
  1. The value is a DEPLOYMENT-WIDE CONSTANT. elitea-main reads it from the
     environment too (`ELITEA_AI_PROJECT_ID`, default 1); it does not vary per
     request, per user or per tenant. Sending a constant per request only
     creates ways for it to be wrong.
  2. It selects a SECOND SCHEMA TO READ. A request-carried value would let
     anyone who can set headers name any project as "public" and read that
     project's rows — a cross-tenant read, and exactly the failure this issue
     warned against. Configuration removes that surface completely.
  3. Carrying it on the identity means changing the HMAC canonical string, which
     both modules must change in lockstep; a version skew either fails every
     request or, if the field is left unsigned, is forgeable. Changing the
     signing scheme is a trust-boundary change (see CLAUDE.md's autonomy
     boundary) and needs a human, which this issue did not ask for.

  *Accepted cost:* the id is set in two places and can drift. A gateway pointed
  at the wrong project serves a model set the UI does not offer. Mitigations:
  the chart documents that the value must match elitea-main's, and main() logs
  the resolved mode at startup — armed with the id, or an explicit warning that
  shared models are unreachable.

  *Default OFF (empty), not 1.* An id naming a schema that does not exist makes
  every credential read fail, so the operator opts in. An unset scope reproduces
  the previous project-local behaviour exactly.

- **Precedence: the caller's OWN row wins.** This matches the legacy resolver
  (`runtime_interface_litellm` `_map_model_name`), which probed
  `{project}_{model}` before `{public}_{model}`. Credentials from the caller's
  project are returned first; on a model-id collision the caller's row is kept
  and the shared row is dropped, so the id appears exactly once. Both rules are
  pinned by tests. NOTE the issue explicitly did NOT decide which credential
  should win where two rows share an id but carry different secrets — bifrost
  picks from the key list we return, and ordering is the only lever this change
  takes. Revisit if product wants shared credentials to override a project's own.

- **Two isolation invariants, and neither may be weakened without a human:**
  1. The public scope is read ONLY with `AND shared = true`. The predicate is a
     constant and is never built from caller-supplied input.
  2. Every row returned from the public scope is re-checked against its own
     `shared` column in Go before use, and the read FAILS if one escapes. This
     mirrors elitea-main's "escaped its authorized scope" check on the same
     table. It exists because the SQL predicate is the kind of thing a later
     refactor drops silently.

  A shared credential's `{{secret.NAME}}` reference resolves against the PUBLIC
  project's Fernet vault, not the caller's — the vault scope follows the
  credential's owner. Resolving against the caller would either fail or pick up
  an unrelated same-named secret of the caller's.

## Topology / build
- Gateway is a standalone Go 1.26.4 module, deliberately OUT of the root go.work
  (which stays 1.25.8 for elitea-main/scheduler). Build with GOWORK=off. Rationale:
  bumping the workspace floor to 1.26.4 broke elitea-main/scheduler CI lint on the
  1.25 runner (learned the hard way in review round 1's fix).

## Prevention gates (CI)
- Three enforcing gates guard the classes that recurred in review:
  1. **Wiring gate** (`cmd/.../main_test.go:TestMainWiring`) — lifecycle methods
     must be called from main.
  2. **Env-drift gate** (`scripts/env-drift-check.sh`) — code env reads must be
     chart-settable or allowlisted.
  3. **Coverage floor** (`scripts/coverage-floor.sh`) — enforcement packages must
     not regress below current coverage.

## Model-name mapping
- **The advertised model id and the provider model name are two different
  names, and the gateway maps one onto the other before dispatch (issue #317).**
  `GET /llm/v1/models` advertises `elitea_title`, a user-authored label. The
  provider only knows the row's `data.name`. The two are independent by
  construction, so the inference path sent the provider a name it does not know.
  LiteLLM did this mapping; nothing replaced it when LiteLLM was removed.

  `internal/llmproxy/modelmap.go` is the single resolution point. Every dialect
  calls `mapModel` after it decodes the request and BEFORE the budget gate. The
  order is load-bearing twice: the provider must never see an unmapped title,
  and the cost tables are keyed by the provider's model name.

- **The provider wire name stays inside the gateway.** `modelObject.providerModel`
  is unexported, so `encoding/json` cannot put it in a caller-facing response.
  Guarded by `TestModelMap_ListDoesNotLeakTheProviderWireName`.

- **The request accepts BOTH the advertised id and the row's own `data.name`.**
  Both name the same configuration row, so both map to the same dispatch. This
  is not a widening: a name that no configured row carries is still rejected.

  *Why both:* every caller that exists today sends `data.name`, not the title.
  elitea-main's model catalog exposes an `llm` row under `data->>'name'`
  (`internal/infra/db/repos/models.go`), the web model picker posts that string
  back as `llm_settings.model_name`, and the e2e seed gives the same row the
  title `e2e-mock-model-llm` and the name `E2E-MOCK-MODEL`. An id-only rule
  would 404 the whole chat path on the first request.

- **An id that matches nothing is 404 at the gateway** (`model_not_found`),
  not an opaque provider error. Guarded by
  `TestModelMap_UnknownModelIs404AndNeverReachesTheProvider`.

- **An UNREADABLE model set forwards the caller's model unchanged.** This is the
  degraded path only: no project on the request, no database, or a query failure
  with nothing cached. The gateway cannot prove the model is wrong, and a 404
  here would turn a database blip into a total inference outage. It is
  deliberately NOT the fail-closed rule the budget path uses — no money and no
  tenant boundary depend on this mapping, and a wrong name fails at the provider
  anyway. Do not change it to fail closed without a human.

  The resolver reports "read the set" and "could not read the set" as different
  answers (`ModelResolver.list`). `List` collapses both to an empty slice, which
  is correct for `/llm/v1/models` and WRONG for dispatch. Keep them distinct.

- **Which name SHOULD be the addressable one is still a product decision.**
  Issue #317 did not pick one, because it changes what external SDK users type.
  This change only makes the list and the request path agree.

- **The shared scope and the name mapping compose in one row loop (#316 + #317).**
  The scope decides WHICH rows come back. The mapping decides WHAT EACH ROW
  CARRIES. They are independent, so `queryScope` builds every row the same way
  in both scopes: a shared row carries `providerModel` exactly like an own row.

  A shared row that carries no provider model name sends the provider a
  user-authored title, and it prices the wrong model at the budget gate, because
  the cost tables are keyed by the provider's name. Both suites are blind to
  this on their own: the #316 tests assert model ids only, and the #317 tests
  seed one scope only.

  `shared_modelmap_test.go` pins the join. It asserts what the PROVIDER
  received, never the status. Three properties are guarded:
  - A shared model dispatches its `data.name`
    (`TestSharedModelDispatchesTheProviderWireName`).
  - On an id collision the CALLER's row supplies the dispatched name
    (`TestCollidingModelIDDispatchesTheOwnRowWireName`). The two rows share an
    id, so the dispatched name is the only evidence of which row won.
  - An unpublished row dispatches under NEITHER of its two names
    (`TestUnpublishedModelIsNotDispatchable`). #317 accepts `data.name` as an
    alias, so the wire name is a second way in if the predicate ever fails.

- **The model set is EVERY addressable configuration section, not the `llm`
  section.** `addressableModelSections` (`internal/llmproxy/models.go`) lists the
  `(section, type)` pairs the resolver reads: `llm`/`llm_model`,
  `embedding`/`embedding_model`, and
  `image_generation`/`image_generation_model`.

  *Why this is a correctness rule and not a preference:* `mapModel` gates EVERY
  dialect against this one set. While the resolver read `llm` rows alone, a
  project's `embedding`/`embedding_model` row was invisible to it, so
  `POST /llm/v1/embeddings` answered 404 `model_not_found` for a model the
  project had configured and whose credential resolved. Measured on the
  standalone stack: the index plane's embedding hop could not dispatch at all.
  `/llm/v1/images/*` had the identical defect. The `llm`-only read predates
  `mapModel` — it was written for `GET /llm/v1/models`, where it was harmless,
  and #317 turned it into a gate without widening it.

- **The fix is in the gateway, NOT in the seed.** Seeding an embedding model as
  an extra `llm`/`llm_model` row also makes it resolve, and it is a smaller
  change. It is rejected because those rows ARE the chat catalogue: elitea-main's
  `/configurations/models/{projectId}` selects `section = 'llm'`
  (`CurrentModelSectionLLM`), which is what the web chat model picker reads. An
  embedding model would become a selectable chat model in the product, for every
  project seeded that way. Keep each model in its own section and make the
  gateway read them all.

- **`asr` and `tts` joined the list with the audio routes (issue #323).** They
  were absent while the gateway mounted no audio route, because admitting a
  section with no route advertises a model no caller can reach. `vectorstorage`
  holds no model and stays out. Add a pair to `addressableModelSections` when,
  and only when, you add a route that dispatches it — `model_sections_test.go`
  covers each pair on its own route.

- **[2026-08-20, issue #323] The gateway serves `/llm/v1/audio/speech`,
  `/audio/transcriptions` and `/audio/translations`.** *Finding:* the retired
  LiteLLM proxy served the first two, and `pylon-indexer`'s voice paths call
  them by absolute URL (`indexer_tts.py`, `indexer_asr_whisper.py`). Nothing
  replaced them, so that image kept a LiteLLM process of its own to answer them
  — a SECOND LLM data plane that applies no budget, bills nothing, and resolves
  credentials from a registry (`runtime_interface_litellm`, in the deleted
  pylon_main) that the platform no longer writes. The model registry it reads is
  therefore empty, so those calls already fail. *Decision:* implement the two
  routes here, on the same order every /llm route follows — decode, `mapModel`,
  `checkBudget`, dispatch, `updateUsage` — so the audio path is governed like
  every other. Two limits are deliberate and are stated in `audio.go`:

  | Limit | Why |
  |---|---|
  | Neither route streams | A streaming speech route needs the detached-drain billing machinery the chat stream has. The pylon TTS client reads the body with `iter_content`, so a unary body still arrives chunked to it; it loses first-byte latency, not audio. |
  | A response the catalog carries no rate for bills zero | `cost.Calculator` now prices three bases — tokens, seconds and characters (migration 0086) — but only from the catalog. There is no default per-second or per-character price and there must not be one: an invented rate reaches the authoritative budget counter as if it were measured. A model with no catalog audio rate is UNPRICED. The condition is counted on `gateway_audio_unpriced_total` and logged, not hidden. |

  **Two follow-up decisions, both from the adversarial review of this change:**

  | Question | Decision |
  |---|---|
  | The widened price `SELECT` names four columns migration 0086 adds. What happens on a pod that rolls out ahead of `elitea-migrate`? | Postgres answers 42703 for EVERY model, and `lookupCatalog` reads any non-`ErrNoRows` error as "uncatalogued" — so the WHOLE catalog would silently bill at the default price table for the length of the skew. `queryCatalog` now catches 42703 alone, latches, and re-reads the same row with the pre-0086 two-column statement. Token pricing survives; only the audio rates go missing, so audio is UNPRICED and counted. The latch expires after 5 minutes and lifts itself when the migration lands. `gateway_price_catalog_schema_behind` (gauge) and `..._total` (counter) are the signal: one for the process, not one log line per model per cache TTL. |
  | The speech route bills a character count bifrost computed from OUR request (`BackfillParams`), not one a provider reported. Is that legitimate? | Yes, and `audio.go` says so plainly. A character-billed TTS provider charges for the input text it was sent, so a rune count of that exact text IS the billable quantity, not an estimate of one. Contrast `BifrostTranscriptionResponse.Duration`, which bifrost DERIVES from word timestamps: that is an observation of something the provider never stated, and `transcriptionUnits` still refuses it. The rule is "bill the quantity the sale is priced on, never an inference about it". Because bifrost backfills that count on every speech response and refuses an empty input, the speech "no usable usage" branch is unreachable through the real router; it is marked as a guard for a non-bifrost router, and the test for that shape now runs against the transcription route, where it is real. |

  **Realtime ASR was NOT covered then. It is now** — see "Realtime sessions"
  below (2026-08-20). `indexer_asr_realtime.py` opens a WebSocket to
  `/v1/realtime`, and the budget and billing design that entry records is what
  the sentence above said was missing.

- **The declared order is the precedence order.** `modelsSQL` joins the pairs
  with `WITH ORDINALITY` and orders by the ordinality before the row id, so a
  model id two sections both carry resolves to the earlier section, and `llm`
  first keeps the chat models in the positions they held before the set grew.
  The pairs travel as bind parameters: no part of the statement text is built
  from the section list.

- **Advertising the other sections on `GET /llm/v1/models` is intended.** OpenAI's
  own `/v1/models` lists embedding and image models, the legacy LiteLLM list did
  too (`preflight.StaticLegacyModels` carries `text-embedding-3-*`, and the BFF.3
  parity gate asserts they stay present), and the web pickers do not read this
  route. It also keeps the invariant `modelmap.go` states: list and dispatch
  agree.

- **[human decision] 2026-08-17 (issue #469) — an unreadable model set REFUSES
  the request. It does not forward the caller's model unmapped.** *Finding:* the
  three conditions in which the resolver cannot read a project's model set all
  produced one outcome: HTTP 200, with the caller's model name sent to the
  provider with no map. The deleted elitea-main handler refused the same
  condition with 502. Pull request #285 reversed that direction and recorded no
  reason. *Decision, per condition:*

  | Condition | Behaviour | Why |
  |---|---|---|
  | Empty project identifier | 404 `model_not_found` | A condition of the request, not a fault. A caller with no project has no configured model and no credential: `GetKeysForProvider` returns zero keys for an empty project. The budget gate also skips a request with no project, so an accepted request would be unmapped AND unmetered. |
  | Nil database handle | 502 `model_catalogue_unavailable` | A wiring fault. `main.go` gives a gateway with no pool NO resolver, and that posture forwards every model unchanged. A resolver that exists with no database can never map anything, so forwarding would make the degraded path permanent. |
  | Query failure, nothing cached | 502 `model_catalogue_unavailable` | A database fault, and the gateway has never read this project's list. |

  *Why no new permissive path is bounded and kept:* the bounded permissive path
  already exists one layer down. A query failure WITH a cached list serves the
  last good list and reports the set as known, so the request maps and dispatches
  as normal (`models.go`, `List`). That list is bounded because every name in it
  came from a real configuration row. The three conditions above are exactly the
  ones in which no list exists, so "permit only a cached name" would permit
  nothing. `TestModelMap_QueryFailureWithACachedListStillDispatches` pins the
  stale path; delete it and a database blip becomes a total outage.

  *Accepted cost:* a project whose model set has never been read gets 502 for
  every request during a database outage, instead of a provider error. The
  counters below make that state visible.

- **A refusal an operator cannot count is a refusal nobody sees.** Each condition
  above increments its own `expvar` counter
  (`gateway_model_map_refused_no_project_total`, `..._no_database_total`,
  `..._lookup_failed_total`). `llmproxy.ModelMapMetricNames` is the ONE named
  path by which they reach `GET /metrics`, so the package that publishes a
  counter also states its name. No name is copied into a second file.

## Realtime sessions
- **[2026-08-20] The gateway serves `/llm/v1/realtime`** (`internal/llmproxy/
  realtime.go`). *Finding:* the audio routes removed two of the three reasons
  pylon-indexer still runs a LiteLLM process of its own; the third is
  `indexer_asr_realtime.py`, which opens a WebSocket to
  `/v1/realtime?model=…&intent=transcription` and pumps PCM16 audio at it. That
  process is a SECOND LLM data plane: no budget, no billing, credentials from a
  registry the platform no longer writes. *Decision:* serve the route here, on
  the same order every /llm route follows — mapModel, then the budget gate, then
  dispatch, then bill.

  **What is different from every other route, and why each difference is a rule
  rather than a preference.** An HTTP request is gated once because it ENDS. A
  realtime session does not end: the tenant holds the socket, the turns keep
  coming, and a hijacked connection has its deadlines CLEARED, so no
  `ReadHeaderTimeout` and no `IdleTimeout` can ever reap it.

  | Property | Rule |
  |---|---|
  | Admission | mapModel → price gate → budget gate, ALL before `websocket.Accept`. After the hijack there is no `http.ResponseWriter` left to write an OpenAI-shaped body with. `checkBudget` was split into `admissionVerdict` (writer-free) and a thin HTTP wrapper so the session re-check asks the SAME question, not a second weaker one. |
  | Billing | Per TURN, never once per session. `updateUsageUnits` → `spawnBillingGoroutine`, which ALREADY mints a fresh uuid per call — so one call per turn is one event id per turn and that function needs NO change. One id per session would make turns 2..N look like redeliveries to the NATS duplicate window and to `gateway.processed_event_ids`' primary key, and they would contribute nothing. |
  | Not `streamSettler` | It ASSIGNS usage, because SSE counts are cumulative over one response. Realtime `response.done` usage is per RESPONSE and must be summed across turns; assigning would bill only the last one. |
  | Keepalive | The gateway's own job, because the hijack cleared the deadlines. Both peers are pinged. |
  | Shutdown | Its OWN phase and its own wait group, NOT the stream drain's (see below). |
  | Read limit | 1 MiB on BOTH sockets. The library default is 32 KiB, realtime frames carry base64 audio well past it, and the failure is a mid-session close with status 1009 rather than an error at setup. |

- **[2026-08-20] The route dispatches the `asr` section. No `realtime` pair is
  added, and that is a finding.** elitea-main writes exactly five model types —
  `llm_model`, `embedding_model`, `asr_model`, `tts_model`,
  `image_generation_model` (`internal/api/v2/configurations/handler.go`) — and
  no `realtime` section exists for a project to put a row in. Declaring one in
  `addressableModelSections` would admit a pair no row can carry, so every
  realtime call would answer 404 for a model nobody could configure. A realtime
  ASR model IS an ASR model, and the `asr` pair joined the list with the audio
  routes. `TestRealtime_DispatchesTheAsrSectionWireName` asserts the pair covers
  the new route, because a missing pair is exactly how `/llm/v1/embeddings`
  broke. Putting a realtime model in the `llm` section is NOT the alternative:
  those rows are the chat catalogue the web model picker reads.

- **[human decision] 2026-08-20 — a mid-session budget-store outage REFUSES THE
  TURN and KEEPS THE SOCKET OPEN. N consecutive outages then close the
  session.** *Decision (H1):* when a live session's re-check returns Block503,
  the turn is not forwarded to the provider and a realtime `error` event is sent
  to the client; the socket stays open. After
  `maxConsecutiveBudgetOutages` = 4 consecutive such re-checks the session is
  closed with status 1008. *Rationale:* fail CLOSED on spend, because no
  un-gated turn reaches the provider for the whole window, without dropping a
  live call on a transient blip. *Why 4:* 4 × the 15 s re-check is about a
  minute — deliberately longer than a NATS reconnect or a leader election, which
  are seconds, and far shorter than the FSM's own continuous-outage ceiling
  (`LLM_BUDGET_NATS_DEGRADED_MAX_DURATION_MIN`, default 10 minutes), so a real
  outage cannot hold an un-gated socket open for as long as that ceiling would
  allow. Turns are refused throughout either way; what the window buys is that
  the caller's session survives a blip. An exhausted budget (402) is NOT a blip
  and closes on the FIRST refusal. The counter counts a 503 and NOTHING else:
  see the 2026-08-20 entry below on the loop breaker.

- **[human decision] 2026-08-20 — an UNPRICED realtime model is REFUSED AT THE
  UPGRADE.** *Decision (H2):* a session opens only when the price CATALOG
  carries a rate for that exact model — a real catalog price, which
  `cost.Cost.FromCatalog()` reports, and NOT a default-table or fallback price.
  Otherwise the gateway answers 502 `model_not_priced` before the upgrade, while
  an `http.ResponseWriter` still exists. Either basis admits: a conversational
  session is sold by the token and a transcription session can be sold by the
  second, and refusing the second shape would refuse every whisper-style model.
  *Rationale:* `cost.Calculator`'s default table PREFIX-MATCHES, so
  `gpt-4o-realtime-preview` resolves onto the plain `gpt-4o` text row and bills a
  confident, roughly 10x-too-small amount that no "unpriced" counter can fire
  on. For a single audio request the answer to an unpriced model is "bill zero
  and count it", and the loss is bounded by one call. A socket the tenant holds
  open has no natural bound, so the same answer caps nothing.
  *Accepted cost:* a project that configures a realtime model the price sync has
  not reached cannot open a session at all, and sees a 502 rather than a working
  call. `gateway_realtime_refused_unpriced_model_total` is the number that makes
  that state visible. On a gateway with NO budget gate wired the check is
  skipped, for the same reason `checkBudget` is skipped there: nothing is metered
  on any route in that posture, and this is not a second policy.

- **[2026-08-20] The mid-session refusal event is a NEW contract, not a
  compatibility requirement.** No elitea-sdk realtime client exists, so nothing
  constrains this shape today; it is written down here so the first client does
  not have to guess. The frame is the provider's own `error` event, and its
  error object carries the EXACT three fields the HTTP budget refusal carries:

  ```json
  {"type":"error","error":{"type":"budget_exceeded","code":"insufficient_quota","message":"…"}}
  ```

  `type` is always `budget_exceeded` and the SCOPE rides in `code`
  (`insufficient_quota` for the project ceiling, `member_budget_exceeded` for
  the member one) — the same rule `budgetErrorType` states for the HTTP path, so
  a future SDK reader reuses the one matcher it already has. A non-budget
  refusal carries its own type and code (`service_unavailable` /
  `nats_unavailable` for a gate outage). A client that only understands the
  provider's `error` event still reads it.

- **[2026-08-20] A realtime session is NOT a stream drain, and shutdown gives it
  its own phase.** `drainWg` / `drainClosing` / `StopStreamGrace` mean "stop
  waiting for a usage trailer on a response that already finished". Their cut is
  one second, which is not a close budget for a live call, and sharing the pool
  would make a session compete for slots with abandoned SSE streams. The session
  group is separate (`sessionWg`, `sessionClosing`, `Handler.
  CloseRealtimeSessions`), with the same Add-after-Wait mutex guard `trackDrain`
  uses. The sequence is:

  `ShutdownHTTP → StopStreamGrace → CloseRealtimeSessions → DrainBilling →
  govStore.Drain → Close`

  AFTER `ShutdownHTTP` because `http.Server.Shutdown` neither closes nor waits
  for a hijacked connection, so nothing else ends a session at all. BEFORE
  `DrainBilling` for the reason the three-phase entry above gives for a
  recovered stream trailer: a session's LAST turn spawns its billing goroutine
  as it closes, and that goroutine needs billing open and NATS live. Move it
  after `DrainBilling` and every session's final turn is refused with
  `billing_refused` on every rolling deploy. `TestShutdownSequence` asserts the
  order by EXECUTING it, and `TestMainWiring` asserts the call exists.

- **[2026-08-20] The accept-side Origin policy is same-origin by default and
  `InsecureSkipVerify` is never used.** A WebSocket handshake is NOT subject to
  CORS, so this is the first /llm path a browser could open cross-site with the
  user's ambient credentials. `websocket.AcceptOptions.OriginPatterns` is left
  EMPTY by default, which is the library's same-origin rule and not "no policy":
  a handshake is admitted when its `Origin` matches the gateway's own host, or
  when it carries no `Origin` at all — which is every non-browser client,
  including the pylon relay this route exists for. An operator widens it, per
  origin, through `LLM_REALTIME_ALLOWED_ORIGINS`, and `logRealtimeMode` states
  the resulting mode at startup so a widened policy is never silent.
  **Still open, and it is NOT in this module:** the edge (elitea-main) performs
  no Origin check of its own, and the gateway's check compares against the
  GATEWAY's host, not the edge's. A browser reaching the route through
  `https://dev.elitea.ai/llm/v1/realtime` therefore has to be admitted by name
  here, which also admits it for a caller that reaches the gateway directly. An
  edge-side check is the right place for a browser-facing policy; it lives in a
  different module and is left as a follow-up.

- **[2026-08-20] The TLS listener advertises `http/1.1` and nothing else.**
  `buildTLSConfig` set no `NextProtos`, and `ListenAndServeTLS` adds `h2` when
  the field is empty — so the listener negotiated HTTP/2 with any client that
  offered it. A WebSocket upgrade needs the raw connection, and net/http gets it
  by hijacking the `ResponseWriter`; an HTTP/2 `ResponseWriter` serves ONE STREAM
  of a multiplexed connection and is not an `http.Hijacker`. RFC 8441 extended
  CONNECT is the HTTP/2 answer and neither net/http nor this gateway implements
  it. Production survived only because elitea-main's proxy transport pins
  http/1.1; a direct in-cluster h2 client failed at an opaque non-Hijacker
  assertion, after the caller believed the handshake had started. Pinning ALPN
  turns that into a clear negotiation failure.

- **[2026-08-20] The dependency is `github.com/coder/websocket` v1.8.15 (ISC).**
  It is net/http-native (`Accept(w, r, opts)` / `Dial(ctx, url, opts)`), every
  operation takes a context, and it serialises writes internally — which is what
  lets the keepalive pinger share a socket with the forwarding goroutine without
  a lock of ours. Its own `require` block is empty, so it costs exactly one
  module. bifrost/core owns NO socket for this surface: there is no realtime
  request method, the provider supplies a URL, a header set and a subprotocol,
  and the CALLER dials, frames and pumps (core's own reference use is
  `internal/llmtests/realtime.go`).

- **[2026-08-20] The route forwards an ALLOWLIST of caller query parameters onto
  the provider URL. `intent` is on it; `model` is not.** *Finding:* the one
  client this route has —
  `legacy/plugins/indexer_worker/methods/indexer_asr_realtime.py` — dials
  `/v1/realtime?model=<m>&intent=transcription`, and `intent` selects the
  provider's TRANSCRIPTION session mode. bifrost's `RealtimeWebSocketURL` builds
  `<base>/v1/realtime?model=<model>` and core@v1.7.3 holds ZERO occurrences of
  "intent", so the parameter was dropped: the provider opened a CONVERSATIONAL
  session for the only caller the feature has. *Decision:* forward the
  allowlisted parameters (`realtimeForwardedParams`, today exactly `intent`),
  never the caller's whole query. A passthrough would let the caller rewrite
  `model` — undoing `mapModel` and the price gate — or name `api-key`,
  `api-version` or `deployment` and pick the credential or the Azure deployment
  the gateway dials. A parameter already on the provider URL is never
  overwritten, so the gateway's own values win whatever the allowlist grows to.
  Add a name only when it selects a provider session MODE.

- **[2026-08-20] A client frame that changes the session model re-runs the WHOLE
  admission sequence, and the session is CLOSED when it fails.** *Finding:*
  `mapModel`, the price gate and `checkBudget` all ran against the `model` QUERY
  parameter, but the model the provider serves is changed by a client frame:
  `session.update` carries `session.model` (`schemas.RealtimeSession.Model` is a
  first-class field) and `transcription_session.update` carries
  `session.input_audio_transcription.model`. The uplink forwarded both verbatim,
  so an admitted, priced model became an UNPRICED one one frame after the
  upgrade, and `billTurn` kept pricing the ORIGINAL. Everything H2 refuses at the
  upgrade was reachable through that hole. *Decision:* re-run `mapModel` → price
  gate → budget gate for the new name; on success ADOPT it (so billing follows
  the model the provider serves) and REWRITE the frame to carry the mapped
  provider name; on failure close the session with 1008 and the OpenAI-shaped
  refusal. *Why not refuse the frame:* `indexer_asr_realtime.py` opens EVERY
  session with `transcription_session.update`, and that frame carries the audio
  format, the language and the VAD settings as well as the model. Dropping it
  leaves the session with no transcription configuration at all — a silent,
  total failure of the only caller. *Why close instead of stripping the model:* a
  session that quietly kept serving the old model would answer a request the
  caller never made, and the caller could not tell.
  A session therefore holds TWO model slots — the response model and the
  input-transcription model — and each turn bills against the model that
  produced it. Pricing a transcription turn with the conversation model's rate
  is a wrong number, not a missing one.

- **[2026-08-20] A live session bounds every gate call itself.** *Finding:*
  `regate` and `gateTurn` called `admissionVerdict` with the SESSION context,
  which has no deadline by design. `admissionVerdict` bounds each store read
  with `budgetGateTimeout` — but that is a context DEADLINE, and a store that
  STALLS while it ignores its context returns nothing at all. The re-check
  goroutine then parked for ever: the ticker never fired again and the session
  ran un-gated for the life of the process. *Decision:* `gateVerdict` runs the
  verdict on its own goroutine and waits `realtimeGateTimeout`
  (`2 x budgetGateTimeout + 1 s`, so a merely SLOW store still answers). A
  timeout is an OUTAGE and decision H1 applies to it: refuse the turn, keep the
  socket, count it toward `maxConsecutiveBudgetOutages`.

- **[2026-08-20] A session ends in ONE order: REFUSE, then CLOSE, then CANCEL.**
  *Finding (F):* the 402 path closed the client socket BEFORE cancelling and
  never set the refusing flag, so the uplink kept forwarding client events to
  the provider for the whole close handshake — which coder/websocket runs for up
  to a HARDCODED 5 s. An exhausted budget paid for five more seconds of
  inference. *Finding (G):* the same order made `Close` wait for the peer's close
  reply while the uplink held the connection's `readMu`; one silent peer burned
  5.001 s of the 5 s `RealtimeCloseTimeout`, and `CloseRealtimeSessions`
  returned with the session STILL LIVE — so `DrainBilling` closed billing while
  that session's last turn was in flight. *Decision:* `realtimeSession.end` sets
  `refusing` FIRST (spend stops at once), then closes, then cancels; and
  `wsSocket.Close` waits `realtimeCloseHandshakeBudget` (1 s) for the peer's
  reply and then walks away. The close FRAME is written before that wait, so the
  caller still learns WHY the session ended.
  **The reviewers' literal prescription — cancel first, then close — was
  MEASURED and rejected.** The session context is the context the uplink reads
  the CLIENT socket with, and coder/websocket arms a `context.AfterFunc` that
  closes the connection ABRUPTLY when it fires (`conn.go`, `setupReadTimeout`).
  Cancel-then-close delivered the close status on 8 runs out of 30 and an
  unexplained EOF on the other 22, so the caller usually could not tell a budget
  refusal from a crash. `end()` records the first reason a session ends, so
  `run()`'s tidy close can no longer race it either.

- **[2026-08-20] The provider is dialled AFTER `websocket.Accept`, and a
  non-upgrade request gets an OpenAI-shaped 426.** *Finding:* the dial ran
  first, so every plain GET, every scan and — because the Origin allowlist lives
  INSIDE `Accept` — every REFUSED cross-site handshake opened one outbound
  WebSocket to a paid provider before it was refused. *Decision:* accept the
  client socket first. The price is that a dial failure has no status line left,
  so it is reported on the socket as the same OpenAI-shaped `error` object every
  other refusal carries, followed by close 1011. The gateway also detects a
  non-upgrade request itself (`isWebSocketUpgrade`) and answers 426 with the
  nested error body, because the library's own 426 is plain text and every /llm
  route refuses in the OpenAI shape (spec 2.5). The client socket is accepted
  with this surface's one subprotocol, `realtime`, because the provider's answer
  is not known yet; the two sides negotiate separately and both are logged.

- **[2026-08-20] Four counters were added because the money path had no signal
  where it needed one.**

  | Counter | The silence it ends |
  |---|---|
  | `gateway_realtime_frames_dropped_total` | `..._turns_refused_total` had its only `Add` in `refuseTurns`, which `regate` calls once per re-check TICK. At the shipped 15 s interval an operator read "1" while a caller streaming 20 audio frames a second had 300 thrown away. The drops are counted where they happen now, and `turns_refused` counts TURN STARTS only. |
  | `gateway_realtime_turn_basis_mismatch_total` | H2's probe admits EITHER basis, and a turn bills on whatever the provider reports. A token-priced model that reports DURATIONS bills nothing, and `updateUsageUnits` answers `billNotBillable`, which no realtime counter saw. The session was H2 admitted and billed zero for its whole life. |
  | `gateway_realtime_turns_unbilled_total` | `billTurn` DISCARDED `billRefused` — real, provider-reported spend dropped because billing was draining. It now publishes `budget.unbilled_stream` with the existing `billing_refused` reason and a `realtime_turn` outcome, exactly as `streamSettler` does, so the one alarm covers both surfaces. |
  | `gateway_realtime_sessions_closed_model_total` | The mid-session model refusal above. |

  `billTurn` also no longer returns silently when the codec cannot DECODE a
  terminal frame. The type is read from the raw frame instead, so such a turn is
  billed when its usage is still readable (`ExtractRealtimeTurnUsage` takes the
  raw bytes) and counted as unpriced when it is not. A turn is also billed when
  the forward to the CALLER fails: the provider had already done the work, and
  returning before `billTurn` was the free-inference-on-disconnect class
  `stream_drain.go` exists to prevent.

- **[2026-08-20] A turn that reports no usable usage is COUNTED, never billed as
  zero.** `ExtractRealtimeTurnUsage` reads `response.usage` off `response.done`
  ONLY. A transcription-intent session emits neither, so bifrost returns nil for
  the event that DOES carry a well-formed top-level `usage` object — this
  gateway parses that envelope itself. Worse, the same extractor returns a
  NON-NIL, ALL-ZERO struct for a duration-shaped envelope, so `if u != nil {
  bill(u) }` bills zero and reports success. Every reader here discriminates on
  the QUANTITY, never on the pointer. A turn with no usable quantity raises
  `gateway_realtime_turns_unpriced_total` and bills nothing; no number is
  invented for it. A `total_tokens`-only envelope is unpriced for the same
  reason: input and output are priced at different rates, so splitting a total
  between them would be a figure the gateway made up.

- **[2026-08-20] A mid-session budget re-check is NOT a request, and it no longer
  feeds the amplification backstop.** *Finding:* `gateVerdict` called
  `admissionVerdict`, and `admissionVerdict` records a hit in the
  per-(project, model) loop breaker. The re-check runs on a ticker for the whole
  life of every session, so the gateway's own gating work arrived in that
  sliding window as if it were tenant traffic. Enough long sessions on one
  project and model opened the circuit for that project's REAL /llm requests —
  an availability defect the gateway caused itself. *Decision:* the verdict
  function takes an `admissionMode`. An ARRIVAL calls `loopBreaker.allow`, which
  reads the circuit and records the hit. A RE-CHECK calls `loopBreaker.observe`,
  which reads the circuit and records nothing. `recheckVerdict` is the re-check
  entry point and the realtime route is its only caller. The re-check still
  RESPECTS an open circuit: dropping the read would put a live session outside a
  control every other path obeys. Tests:
  `TestRealtime_TheBudgetRecheckIsNotCountedAsARequest`,
  `TestRecheckVerdict_ObservesTheBackstopWithoutFeedingIt`,
  `TestLoopBreaker_ObserveReportsTheCircuitAndRecordsNothing`.

- **[2026-08-20] Decision H1's outage counter applies to a 503 ONLY.**
  *Finding:* `regate`'s default branch counted EVERY non-Allow, non-402 verdict
  toward `maxConsecutiveBudgetOutages`. H1 was authored for one condition — the
  budget store did not answer — and a tripped loop breaker is a different
  condition with a different lifetime (`DefaultLoopBreakerOpenFor` is 5 s). A
  live call was therefore torn down under a policy nobody wrote for it, and for
  a condition that had usually already cleared. *Decision:* the branches are
  separated. A 503 refuses the turn, keeps the socket and counts toward H1. Any
  other refusal refuses the turn, keeps the socket and counts NOTHING — the
  counter is left unchanged rather than reset, because such a verdict says
  nothing about whether the store is reachable. Only an Allow ends an outage
  run. Test: `TestRealtime_ATrippedBackstopRefusesTurnsButKeepsTheSession`.

- **[2026-08-20] The upstream reports the NEGOTIATED subprotocol, not the
  declared one.** *Finding:* `RealtimeUpstream.Subprotocol` carried
  `RealtimeWebSocketSubprotocol()`, which is only what the dial OFFERED. A
  provider is free to answer with none, so the session log named a negotiation
  that may not have happened. *Decision:* read the value off the opened
  connection (`conn.Subprotocol()`). Test:
  `TestOpenRealtimeSocket_ReportsTheNegotiatedSubprotocol`.

- **[2026-08-20] Each keepalive ping gets its OWN deadline.** *Finding:* the two
  pings shared one context and ran one after the other. `coder/websocket`'s
  `Ping` waits for the pong, so a caller that was slow to answer spent the whole
  shared budget; the provider ping then started on an expired context, answered
  "context deadline exceeded", and the session was torn down on a FALSE liveness
  verdict about a healthy provider. *Decision:* `pingPeers` pings both peers at
  the same time, each under its own deadline (`realtimePingBound`, default
  `realtimeWriteTimeout`). The two sockets are different connections, so a
  concurrent ping on each is safe. Test:
  `TestRealtime_EachKeepalivePingGetsItsOwnDeadline`.

- **[2026-08-20] The production dialer is tested, and the split that made it
  testable.** *Finding:* `DialRealtime` and all four `bifrostRealtimeCodec`
  methods measured 0.0% coverage, because every realtime test injects a fake
  dialer. The one piece of the route that touches the bifrost realtime API — an
  API nobody here had used before — was the one piece nothing ran. *Decision:*
  `openRealtimeSocket` holds everything below the key selection (URL, headers,
  dial, read limit, negotiated subprotocol); it takes the two optional provider
  interfaces, so a stub `schemas.RealtimeProvider` and a real WebSocket listener
  drive all of it. `DialRealtime` itself is also driven end to end against a
  real embedded core whose account points one provider at a test listener
  (`TestDialRealtime_ThroughTheEmbeddedCore`). Coverage went 0.0% → 100%
  (`openRealtimeSocket`, all four codec methods) and 86.7% (`DialRealtime`; the
  two remaining statements need a provider that implements one optional
  interface but not the other). The provider-side `SetReadLimit` is pinned by
  `TestOpenRealtimeSocket_PinsTheProviderSideReadLimit` — deleting the call left
  the whole suite green before.

- **[2026-08-20] A URL parse failure no longer repeats the URL.** *Finding:*
  `realtimeProviderURL` returned `url.Parse`'s own error, and that error QUOTES
  the URL it failed on. The base URL comes from the credential and can carry
  userinfo, so the one function every caller is careful not to log put the URL
  back into the error text. *Decision:* return the REASON only. Test:
  `TestOpenRealtimeSocket_UnparsableProviderURLIsReported`.

## Observability
- **[human decision] 2026-08-17 (issue #465) — `GET /metrics` serves a named
  allowlist. `/debug/vars` stays unpublished.** *Finding:* the
  `gateway_budget_enforcement_enabled` gauge had no route for its whole life.
  `expvar` registers `/debug/vars` on `http.DefaultServeMux`, and this process
  serves its own multiplexer. The comment said operators could alarm on the
  value 0, and no operator could read the value at all. This mattered most for
  issue #304: a gateway that starts while NATS is unreachable enforces nothing
  for the life of the process, and this gauge is the control that reports it.

  *Decision:* mount `/metrics` on the gateway multiplexer, in the Prometheus
  text exposition format, serving the variables `gatewayMetrics` names and
  nothing else. `expvar.Handler()` is NOT used: it writes every variable the
  process publishes, `cmdline` (the process arguments) and `memstats` included,
  on the same listener that serves `/llm`. The wiring gate in `main_test.go`
  forbids the call, and requires the `mux.Handle("/metrics"` mount.

  *Exposure:* the shipped Service is ClusterIP with mutual TLS, and the edge
  proxies only the `/llm` paths, so `/metrics` is reachable from inside the
  cluster and not from a tenant.

  *Proof rule for this route:* a test that reads the variable in the same
  process does not prove a route exists. `TestMetricsRoute_IsServedByTheRunningGateway`
  builds the binary, starts it, and scrapes it over HTTP. Remove the mount and
  that test answers 404, while every handler-level test still passes.

- **[human decision] 2026-07-26, implemented 2026-08-28 (issue #17) —
  `X-Elapsed-Ms` measures INSIDE the router, up to the resolved credential.**
  *Finding:* the header fed the BFF.9d overhead gate (design §10.2) with
  PRE-DISPATCH time alone. It missed the per-request credential resolution —
  the Account's Postgres read plus the Fernet decrypt — and it missed core
  routing, both of which §10.2 counts as gateway overhead. The staging result
  (p99 17.1 ms) therefore understated the real cost.

  *Decision:* "Move measurement inside the router." bifrost v1.7.3 gives no
  seam around the provider round-trip (`ProviderConfig.NetworkConfig` takes no
  RoundTripper), so "overhead = total minus provider" stays unavailable. The
  boundary is the credential instead: core dials the provider only after it
  holds the key.

  *Mechanism:* the /llm Chat handler puts an `overhead.Meter`
  (internal/overhead) on the `BifrostContext` before dispatch.
  `account.GetKeysForProvider` marks it, because bifrost/core passes the
  caller's own context to that method. The handler stamps the header AFTER the
  router returns — after `ChatCompletionRequest`, and before `beginStream` on
  the streaming path. The Meter keeps the EARLIEST mark: a later mark comes
  from a retry or a fallback, which runs after a provider round-trip. Marks are
  atomic, because core resolves credentials from its provider workers.

  *What the header now covers:* decode, identity verification, loop breaker,
  budget check, the wait in the core provider queue, core routing, and the
  credential read. *What it still excludes:* the provider round-trip and the
  gateway's own response marshalling. A request that resolves no credential (a
  direct key, a plugin short-circuit) reports the pre-dispatch time alone.

  *Consequence:* the number gets LARGER, and it now moves with Postgres and
  with core queue depth. `internal/llmproxy/testdata/p99_overhead_benchmark.json`
  records the OLD, narrower metric; do not read it as a measurement of this one.

## Resolved follow-ups
- ✅ `SECRETS_MASTER_KEY` + `GATEWAY_IDENTITY_SECRET` now wired via the chart's
  `secrets:` block (valueFrom.secretKeyRef, optional:true default) — provision the
  `elitea-llm-gateway-secrets` Secret out-of-band. env-drift gate updated to read
  `.secrets` keys; allowlist entries removed.
- ✅ `internal/llmproxy` coverage raised 84.2% → 91.5% (coverage_boost_test.go);
  coverage-floor bumped to 85.
- ✅ `/llm/v1/messages/count_tokens` now budget-gated (was the 4th "unmetered path"
  found — by dogfooding the saved gateway-review workflow on PR#6, after 3 manual
  rounds missed it). `checkBudget(...,0)` before `CountTokensRequest`; no updateUsage
  (count_tokens has no billable usage). Guarded by TestBudgetGate_CountTokens_Block402.
- ✅ Wiring gate now guards `llmproxy.WithBudgetGate(` — the point that attaches the
  gate to the handler was the single most critical UNasserted wiring; mutation-verified.
- ✅ env-drift regex `[a-zA-Z]+Or` → `[a-zA-Z0-9]+Or` (was blind to `uint32Or`, so the
  `LLM_BUDGET_CB_*` vars were silently skipped); CB vars added to values.yaml.
- ✅ (elitea-platform storage-migration-plan.md S17) env-drift-check.sh's own
  `_test.go` exclusion was a no-op: `grep -rho` strips filenames before the
  next stage's `grep -v '_test.go'` filter ever sees them, so every
  integration-test-only env var (e.g. `ELITEA_TEST_DATABASE_URL`) was
  misreported as a production FAIL for BOTH services. Fixed by dropping `-h`.
  Also added detection for elitea-main's `lookup("VAR")`-parameter indirection
  (storage.ConfigFromEnv's injectable-lookup pattern, distinct from the
  existing `fooEnv = "VAR"` const-indirection case), which no existing
  pattern matched — every var read that way was misreported as dead chart
  config. elitea-main's chart now wires the S17 storage vars (S2/S5/S12) for
  real and the allowlist was trimmed to match; the check now also runs as
  its own `env-drift` job in `ci-go.yml` (previously it was only reachable
  via `ci-gateway.yml`'s path-scoped trigger, which never fires for an
  elitea-main-only or chart-only change — see the next line).

## Known follow-ups (not blocking, need a human)
- Re-baseline the BFF.9d threshold against the wider `X-Elapsed-Ms` (issue
  #17). 50 ms was set against the pre-dispatch-only metric. The new value adds
  a Postgres round-trip, a Fernet decrypt and the core queue wait, and a cold
  pool can exceed 50 ms. The number needs a k6 run on staging
  (`cmd/cutover-ctl/testdata/overhead_loadtest.js`); it cannot be measured from
  a unit test, so the threshold is unchanged for now.
- ~~Set `secrets.*.optional: false` ... for `GATEWAY_IDENTITY_SECRET`~~ — DONE
  2026-08-09 (issue #11, see the Trust-boundary entry above): it is `false` in
  the base chart for both the gateway and elitea-main. `SECRETS_MASTER_KEY`
  remains `optional: true` (unset ⇒ Fernet vault degraded single-level mode);
  making that one mandatory is still open and needs a human.
- elitea-main env-drift is still WARN-heavy for vars read via a default
  (`ELITEA_RUNTIME_ENABLED`, `REDIS_URL`, and ~19 others) with no chart
  override knob at all — real, now-visible (the two bugs above previously
  hid most of it), and left as accepted debt: wiring 20+ unrelated knobs
  into values.yaml was out of scope for the storage-migration work that
  surfaced this. WARN does not fail CI.
