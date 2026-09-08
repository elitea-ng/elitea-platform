# CLAUDE.md — elitea-llm-gateway

Standing rules for any agent working in this module. These are decisions already
made (see DECISIONS.md for rationale) — apply them WITHOUT re-asking. They exist
because each was a real bug found across 3 review rounds.

## Build / test (non-negotiable)
- This is a **standalone Go 1.26.4 module, NOT in the root go.work.** ALWAYS build
  and test with `GOWORK=off` (e.g. `GOWORK=off go test -race ./...`,
  `GOWORK=off golangci-lint run ./...`). Plain `go test` from the workspace fails.
- Before claiming done, BOTH must be green: `GOWORK=off go test -race ./...` AND
  `GOWORK=off golangci-lint run ./...`. `go vet`/`go test` alone is not enough —
  CI runs golangci-lint and it has caught real bugs (ineffassign, errcheck).

## Money path (correctness-critical)
- Money is **int64 nano-USD end to end**. NEVER introduce a float on the money
  path. Prices are **per-1M tokens** (guard the 1000× denomination bug).
- Use `math/big` where an int64 multiply could overflow; guard `big.Int.Int64()`
  with `IsInt64()`.
- **A cut stream settles from the provider's ACCUMULATED count, read off the
  context handle** (`schemas.BifrostContextKeyStreamAccumulatedUsage`,
  `streamSettler.accumulatedUsage`, issue #79). Three rules hold that path
  together, and each one was a real defect first:
  1. Read the HANDLE, never the cancellation chunk. bifrost delivers that chunk
     with a `select` against a context the gateway has already cancelled, so
     both cases are ready and delivery is a coin flip (~half the streams).
  2. Reject a count at or below `placeholderOutputTokens`. Anthropic's
     `message_start` reports `output_tokens: 1` before it generates anything,
     and the accumulator MAX-merges, so a mid-stream cut sees that value
     unchanged. Billing it is an ~89% underbill reported as a success.
  3. A partial bill STILL publishes `budget.unbilled_stream`, with reason
     `partial_provider_usage`. A partial count is a floor, not the total.
  Precedence is terminal trailer > partial > nothing, and it only moves up.
  Read the handle ONLY after the provider closed the channel: the provider
  mutates it in place, and the close is the sole happens-before edge.

## Enforcement policy (do not weaken without a human)
- **Fail CLOSED** on a budget-store / Postgres read error while NATS is up — never
  substitute a zero/unlimited snapshot. A DB blip must not grant unlimited spend.
- **The gateway REFUSES TO START when it can enforce nothing and nothing else
  would say so** (issue #304, `cmd/elitea-llm-gateway/budget_startup_gate.go`).
  `LLM_BUDGET_REQUIRE_ENFORCEMENT` selects the posture: `auto` (default) refuses
  when `GATEWAY_NATS_URL` is empty AND the database holds an enforcing budget
  row, `on` refuses whenever the gate is not wired, `off` is the documented
  opt-out. Do NOT make `auto` refuse a CONFIGURED-but-unreachable NATS: that
  state drains through `/readyz` and recovers by itself (issue #315). Do NOT let
  a probe that FAILED to read the budget tables be read as "no budgets".
- Budget gate runs **before** the provider on EVERY /llm endpoint (chat,
  responses, text, embeddings, images, messages). Adding a new endpoint? It MUST
  call checkBudget before dispatch and updateUsage after.
- **Read the budget path with `h.budget()`, ONCE per operation.** The gate, the
  price calculator, both event publishers and the `budget_used` reader are one
  immutable value behind one `atomic.Pointer`
  (`internal/llmproxy/budget_plane.go`), because a gateway that boots during a
  NATS outage installs them later, while it serves traffic (issue #315). Do NOT
  add a plain `Handler` field for anything on that path, and do NOT load the
  plane twice in one operation — pass the snapshot down, as `admissionVerdictFor`
  and `spawnBillingGoroutine` do. `InstallBudgetEnforcement` installs ONCE: it
  refuses a nil gate and refuses to replace one. Keep both refusals; they are
  what makes the snapshots safe and what stops a transport error from turning
  enforcement off.
- The model a caller sends is **mapped to the provider's own model name before
  dispatch** (`mapModel`, internal/llmproxy/modelmap.go). Adding a new endpoint
  that carries a model? It MUST call `mapModel` after the decode and BEFORE
  `checkBudget` — the provider must not see an unmapped id, and the cost tables
  are keyed by the provider's name.
- An **unreadable model set REFUSES the request** (issue #469, 2026-08-17; this
  line replaces the earlier "do not fail closed without a human"). The three
  conditions get three behaviours: an empty project identifier answers 404
  `model_not_found`; a nil database handle and a query failure with no cache
  answer 502 `model_catalogue_unavailable`. Do not make any of them forward the
  caller's model unmapped again without a human (DECISIONS.md, "Model-name
  mapping"). The permissive path is the STALE CACHE in `List`: a query failure
  with a cached list still maps and dispatches, so a database blip is not an
  outage. Do not delete that path.
- `mapModel` gates every dialect against ONE model set, built from the
  `(section, type)` pairs in `addressableModelSections`
  (internal/llmproxy/models.go). Add the pair there when you add a route that
  dispatches a new kind of model, and add its case to
  `addressableSectionCases()` in `model_sections_test.go`. A missing pair makes
  the gateway answer 404 `model_not_found` for a model the project configured
  correctly — that is how /llm/v1/embeddings and /llm/v1/images/* broke. Do NOT
  work around it by seeding the model as an `llm`/`llm_model` row: those rows are
  the chat catalogue the web model picker reads.
- Error bodies are **OpenAI-shaped on ALL /llm routes** (spec §2.5): nested
  `{"error":{"message","type","code"}}`. 402=budget_exceeded/insufficient_quota,
  429=rate_limit_error/rate_limit_exceeded, 503=service_unavailable/nats_unavailable.
  Do NOT emit Anthropic-shaped errors on /llm/v1/messages.
- **A realtime session is gated MORE than once.** `/llm/v1/realtime` hijacks the
  connection, so every admission step (mapModel, the price gate, `checkBudget`)
  MUST run before `websocket.Accept` — after it there is no `http.ResponseWriter`
  to refuse with. A live session re-asks the SAME verdict on a ticker
  (`recheckVerdict`, `LLM_REALTIME_BUDGET_RECHECK_SEC`); that ticker is the
  MANDATORY bound, because bifrost's turn-start signal is one the only known
  caller never sends. Use `recheckVerdict` and NOT `admissionVerdict` for any
  mid-session gate call: `admissionVerdict` records a hit in the loop breaker,
  and a ticker that records one turns the gateway's own gating work into tenant
  traffic (DECISIONS.md, 2026-08-20). Bill per TURN, never once per session. An UNPRICED
  realtime model is refused at the upgrade (DECISIONS.md, human decision H2) and
  a mid-session gate outage refuses the turn while keeping the socket open (H1).
  H1's consecutive-outage counter counts a 503 and NOTHING else; a refusal that
  is not an outage refuses turns and keeps the socket without counting.
  Do NOT reuse `streamSettler`, `drainWg`, `drainClosing` or `StopStreamGrace`
  for it — DECISIONS.md "Realtime sessions" says why each is wrong.
- **Admission does not end at the upgrade.** A client `session.update` or
  `transcription_session.update` can change the model the provider SERVES, so
  `admitFrameModels` re-runs mapModel, the price gate and the budget gate for
  the new name, adopts it when it passes, and closes the session when it does
  not. Keep it: without it, H2 is bypassed one frame after the handshake. The
  provider URL receives only the ALLOWLISTED caller query parameters
  (`realtimeForwardedParams`; `intent` is on it, `model` is never).
- **A session ends through `end()` only, in the order refuse → close → cancel.**
  Do NOT reorder it. Cancelling first destroys the close frame, because
  coder/websocket closes the connection abruptly when the read context fires,
  and the caller then cannot tell a budget refusal from a crash (measured: 8
  close frames delivered out of 30). Every mid-session gate call goes through
  `gateVerdict`, which bounds it: a budget store that STALLS rather than fails
  would otherwise park the re-check goroutine for ever and leave the session
  un-gated.
- A budget refusal puts the SCOPE in `error.code` and always keeps
  `error.type = "budget_exceeded"` (`budgetErrorType` /  `budgetCodeProject` /
  `budgetCodeMember` in internal/llmproxy/budget_gate.go). elitea-sdk matches on
  the type alone, then reads the scope from the code. A refusal typed with its
  scope is not read as a budget refusal at all, and the SDK then feeds the
  policy rejection back to the model as message content. The member ceiling is
  the one refusal whose code is not the OpenAI canonical one; the project
  ceiling keeps `insufficient_quota` because the cutover gate asserts it and the
  SDK resolves an unknown code to the project scope.
- **Every ceiling that refuses sends a soft alert first.** `trySoftAlert` is
  scope-generic (issue #510): pass `scope` + `scopeID`, and run it for the
  member ceiling as well as the project one. Give each scope its OWN
  pre-increment snapshot; the crossing test compares that scope's before with
  that scope's after. Add a new ceiling? It MUST get a crossing check too.
  Publish the event on the PROJECT's subject for every scope —
  `PublishSoftAlertEvent` builds `gateway.events.project.<id>.events` from its
  first argument, so a member scope_id there reaches nobody. Put which budget
  crossed in the payload (`scope`, `scope_id`, `user_id`).

## Security / trust boundary
- **CORS does not apply to a WebSocket handshake.** `/llm/v1/realtime` is the
  first /llm path a browser could open cross-site. The accept-side Origin
  allowlist (`LLM_REALTIME_ALLOWED_ORIGINS`) is EMPTY by default, which is the
  same-origin rule and not "no policy". NEVER set `InsecureSkipVerify`.
- The edge (elitea-main) trusts `X-Auth-*` / `X-Elitea-*` ONLY from a configured
  trusted proxy (`TRUSTED_PROXY_CIDRS`, checked against RemoteAddr, deny-by-default).
  Strip client-supplied Cookie/Authorization/X-Api-Key/X-Auth-*/X-Elitea-* before
  proxying; the gateway sees only edge-signed identity.
- Never log credentials/tokens/master keys or a URL containing userinfo (redact).
- **The hop marker is the loop detector; the loop breaker is not** (issue #164).
  The gateway sets `X-Elitea-Llm-Hop` on every outbound provider request
  (`internal/account`) and refuses an inbound request that carries this
  deployment's own marker (`internal/llmproxy/hopguard.go`, mounted on the
  ROUTER ROOT in `internal/api/router.go`). Keep three rules:
  1. Key it from `GATEWAY_HOP_SECRET` ONLY. Never use `GATEWAY_IDENTITY_SECRET`
     and never derive one from the other. The marker goes to every upstream, and
     a provider `api_base` is tenant-authored.
  2. Keep detection STATELESS. Read the header, compare it, refuse that request,
     record nothing. Any upstream can harvest the marker, so a refusal that fed
     a counter or opened a circuit would become a cross-tenant denial of
     service.
  3. Mount it on the router root, not per route. A loop can aim at a path the
     gateway does not serve, and a 404 never reaches admission.
  The browser edges delete every other `X-Elitea-*` name; this one is exempt on
  purpose. `mustForwardHeaders` in `services/elitea-main/tests/deployedge/
  edge_identity_strip_test.go` holds the exemption, and
  `internal/hopmarker/path_pin_test.go` fails when the two modules drift.

## Wiring (the bug class that recurred 3×)
- Any lifecycle method (Start/Drain/DrainBilling/Close) that must run to be
  effective MUST be called from the composition root (`cmd/.../main.go`) AND
  guarded by the wiring test in `cmd/elitea-llm-gateway/main_test.go`
  (`TestMainWiring`). A unit test proving the method works in ISOLATION does not
  prove main() calls it. When you add such a method, add its assertion there.
- Any env var the code reads (config.go / vault.go) MUST either be settable by the
  Helm chart (`deploy/helm/elitea-llm-gateway/values.yaml`) or be on
  `scripts/env-drift-allowlist.txt` with a justification. `scripts/env-drift-check.sh`
  enforces this in CI.
- An operator control MUST have a route. A published `expvar` variable has none:
  `expvar` registers `/debug/vars` on `http.DefaultServeMux`, and this process
  serves its own multiplexer (issue #465). Add the variable to `gatewayMetrics()`
  in `main.go`, and it reaches `GET /metrics`. Do NOT mount `expvar.Handler()`:
  it publishes every variable, `cmdline` and `memstats` included, and
  `TestMainWiring` forbids the call. Prove such a route with an HTTP request to
  a RUNNING gateway; a test that reads the variable in the same process proves
  nothing.

## elitea-sdk compatibility (the defect that shipped)
- The /llm surface has ONE first-class client, and the gateway is pinned to it.
  `internal/sdkpin/sdk-pin.json` names the elitea-sdk revision the compatibility
  gates were last verified against. The revision itself is owned by
  `services/elitea-worker-python/elitea-sdk.lock.json`; `internal/sdkpin/
  pin_test.go` compares the two, so an SDK bump turns this module's test job red
  with no gateway source change. Do NOT update the pin to make that job green —
  re-run the two gates first.
- The budget refusal contract is: error.TYPE is always `budget_exceeded`, and
  error.CODE carries the SCOPE. The SDK's `budget_exceeded_from` matches on the
  type ALONE and reads the scope from the code. A refusal that puts the scope in
  the type is not recognised as a budget refusal at all: no typed exception is
  raised and the policy rejection reaches the model as message content. That
  shipped, and every Go test passed while it did.
- THREE tiers guard it, and each sees what the one below cannot:
  - tier 1 `scripts/contract/test_sdk_budget_contract.py` — reads both sources
    and runs the SDK reader. It reads the CONSTANTS, so it cannot see which
    constant a call site uses.
  - tier 2 `scripts/sdk-conformance/run.sh` — drives the installed SDK against
    `api.NewRouter` over the real handler and produces REAL 402s. This is the
    tier that catches a swapped constant, a 404 route and a lost stream trailer.
  - tier 3 `deploy/scripts/sdk-client-check.sh` — the SDK against a whole
    standalone stack. Operator-run; that stack cannot produce a 402.
- A new /llm route, a change to the refusal body, or a change to
  `internal/api/router.go` MUST be followed by a tier 2 run. `.github/workflows/
  ci-python.yml` starts tiers 1 and 2 on every change under this module.
- EACH TIER HAS A FLOOR, and the floors are load-bearing. Tier 1 checks that
  its own tests are still collected (`EXPECTED_TESTS`), tier 2 refuses a run
  reporting fewer than `EXPECTED_ASSERTIONS`, and tier 3 does the same and
  prints the count its caller reads. Do not lower a floor to make a job green.
  Every one of them was added because the gate reported success while
  measuring less than it claimed: a deleted test file left the file count on
  its old floor, a renamed test silently removed half a gate, and a wrapper
  replaced by `exit 0` passed every structural check CI made.

## Language (ASD-STE100)
Write ALL agent-authored text in ASD-STE100 Simplified Technical English: GitHub
issues, PR bodies, review comments, commit messages, and documentation edits.
Rules that apply here:
- Write short sentences (max 20 words for an instruction, max 25 for a description).
- Give one instruction per sentence. Use the active voice and the present tense.
- Use one approved term for one thing; do not switch between synonyms.
- Start an instruction with the verb (e.g. "Set GOWORK=off before you build").
Code identifiers, file paths, log excerpts, and quoted output are exempt.

## Autonomy boundary (for agent-driven review/fix loops)
An agent may run review→fix→verify→push autonomously ONLY within these limits:
- Fix only findings tagged **autoFixable** (mechanical: wiring, error handling,
  test strengthening, doc/lint). 
- **NEVER** change, autonomously, any of: the fail-open/closed policy, the trust
  boundary / CIDR model, the money denomination, the async-billing overshoot
  bound, or anything in DECISIONS.md marked "human decision". Escalate those.
- Only advance while CI stays green; if a gate goes red, stop and report.
- Verify every fix against its own gate (`-race`, lint, the wiring/env/coverage
  gates) before committing — do not trust a subagent's self-report.
