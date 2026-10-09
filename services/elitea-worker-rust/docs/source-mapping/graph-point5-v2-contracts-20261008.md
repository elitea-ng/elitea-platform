# Point 5 Wave 1 Track D — contracts and design documents (2026-10-08)

Branch `docs/graph-point5-v2-contracts` from `origin/main`. Documentation, JSON schemas, fixtures and one Go
contract test. No runtime code. All admission gates stay `false`.

This file links the new documents and is listed in `source-mapping/README.md` (one line appended at the end of the
file, away from the hunks #1084 adds there). `remaining-gates.md` and `testing-gaps.md` are owned by
PR elitea-ng/elitea-platform#1084 until it merges and are not edited here.

## Business behaviour taken from the current platform, and what was not ported

Taken (reference: EliteaUI `src/[fsd]`, Python SDK, Pylon plugins):
- HITL cards look and behave as in EliteaUI: `AnswerApprovals` below the thinking view, one accordion per spawning
  instance bucketed by `parent_agent_call_id`, labels from `computeBreadcrumbs` (`Parent (1) ▸ Sub`). Fan-out members
  reuse this hierarchy contract; `fanout_v1` adds grouping metadata only (D2 §3.3–3.4).
- The action vocabulary per card kind (approve, reject, edit, block_with_comment, answer, authorize, skip).
- Independent nested sub-agent HITL: each card resolves on its own and siblings keep running.

Deliberately not ported:
- The legacy async supervisor (parent parks, children as separate durable tasks, reconcile RPC, replay). It caused
  concurrent pause loss, non-converging multi-round replay and phantom UI instances. V2 uses one in-process runner and
  proves each decision on the child checkpoint (D1 §5, §15).
- The new platform's own complete-decision-set resume (Worker, Main Go and SQL) is superseded (D2 §10, D4).
- Grouping sub-agent instances by name (the phantom-instance bug class): members are identified by a call id derived
  from the frozen child identity (D2 §3.3).

## Deliverables

| Brief item | File(s) | Notes |
|---|---|---|
| D1 Fan-out V2 runtime design | `services/elitea-worker-rust/docs/fanout-v2-runtime-design.md` | One `FanoutRunner` for Parallel and Map; frozen child identity; ≤2 parent rows per activation; per-interrupt apply proven on the child checkpoint; live + parked triggers; failure, budgets, sequence, crash-window matrix, delete-list. |
| D2 Interrupt decision contract | `libs/proto/contracts/fanout-interrupt-decisions-v1.md`; `libs/jsonschema/runtime/v1/fanout-*.schema.json` (15); `libs/jsonschema/runtime/v1/fixtures/fanout-*` (28 valid, 46 invalid) | Card, ledger, public decision API, private fetch/ACK, park/wake, frames, hierarchy (`Parent (1) ▸ Sub`), key/digest vectors. Track M2 implements the ledger and API from it. |
| D3 Map V1 rewrite | `services/elitea-worker-rust/docs/map-reduce-pipeline-node-design.md` | Describes the shipped V1 code; V2 items point to D1. |
| D4 Parallel doc superseded | `services/elitea-worker-rust/docs/parallel-pipeline-node-design.md` | Complete-set sections marked superseded, history kept. |
| D5 HTTP action node design | `services/elitea-worker-rust/docs/http-action-node-design.md`; `libs/jsonschema/runtime/v1/http-action-*.schema.json` (5); fixtures `http-action-*` (9 valid, 17 invalid, 25 render vectors) | Revision 2 operation + bindings, Main renders, invocation v3, effects, rules by project admins, egress, content pool, crash cases. |
| D6 Database action node outline | `services/elitea-worker-rust/docs/database-action-node-design.md` | Wave 3 outline. |
| Schema validation in CI | `services/elitea-main/internal/runtimecontracts/schemas_test.go` | Runs in the Go CI job (`go test`). |

## Conformance test

`go test ./internal/runtimecontracts/` (module `services/elitea-main`) checks every `fanout-*` and `http-action-*`
schema with the existing `github.com/santhosh-tekuri/jsonschema/v6` dependency:
1. Draft 2020-12 compile, `$ref` by `$id`, `$id = elitea.<area>.<stem>.v<N>`.
2. Strictness lint: closed objects, `maxLength`, `maxItems`, `minimum`/`maximum` on every typed subschema.
3. ≥1 valid and ≥2 invalid fixtures per schema; valid pass, invalid fail; a family-prefixed fixture with a malformed
   name fails the test instead of being skipped.
4. Fixtures are byte-canonical JSON (the canonical rule shared by D2 §7 and D5 §6; the test has a reference writer
   because Go `encoding/json` always escapes U+2028/U+2029).
5. Recomputes `interrupt_key`, member `call_id`, `decision_sha256` and the HTTP render vectors' body and request
   digests.

Evidence on 2026-10-08 (darwin/arm64, go1.26.5): `go vet` clean; `go test -count=1 -v ./internal/runtimecontracts/`
4/4 PASS, 0 skips (`TestRuntimeContractSchemasAndFixtures`, `TestFanoutInterruptKeyAndMemberCallIDVectors`,
`TestFanoutDecisionDigestsInFetchFixtures`, `TestHTTPActionRenderVectorsAreSelfConsistent`). Mutation check: a
non-canonical fixture, a corrupted valid fixture, an extra field, a wrong key digest, a misnamed fixture and an
unbounded property named `default` each made the test fail. Independent cross-check with Python `jsonschema` 4.26
(Draft202012Validator + referencing registry): 102/102 fixtures behave as intended. `golangci-lint` was not run locally (not installed); CI runs it.


Known gap (CI unchanged by user decision): `ci-go.yml` triggers on `services/elitea-main/**` and `libs/proto/**`, not
on `libs/jsonschema/**`. A schema- or fixture-only change does not start the Go job by itself; such a change must also
touch the contract document under `libs/proto/contracts/` (which it should anyway) until CI owners add the path.

## How the fixtures were created

All fixtures are synthetic; none comes from a UI session or a database. The `fanout-*` schemas and fixtures were
written by a one-off Python generator (each invalid fixture is one valid fixture with exactly one defect; digests
computed with `hashlib`), and the `http-action-*` render vectors by a Python reference renderer using the Worker
`encode_component` character set. The generators are not committed: the Go test recomputes every digest
independently, so a hand edit that breaks a fixture fails CI.

## Performance

This PR ships no runtime code, so it enforces no runtime budget itself. Every budget is assigned a named mechanism and
a named proving test in the designs, to be built and measured by the implementing tracks: D1 §15 Performance table
(parent rows, 2-transaction prepare, coalescer, decision tick, load percentiles), D2 §12 Performance table (single
decide transaction, POST p95 ≤ 150 ms, fetch caps), D5 §16, D6 §12.

| Rule this PR enforces | Mechanism | Test | Result |
|---|---|---|---|
| The contract check stays cheap | one in-process compile of 20 schemas, no network | `TestRuntimeContractSchemasAndFixtures` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:289`) | 0.2–0.3 s locally |

## Durability

Crash windows and recovery rules: D1 §8 and §15 Durability table (named PG and crash-seam tests per window), D2 §12
Durability table, D5 §13 (18 cases, each with a named test in §16), D6 §12.

| Rule this PR enforces | Mechanism | Test | Result |
|---|---|---|---|
| Recovery identities are reproducible across languages (`interrupt_key`, member `call_id`) | length-prefixed SHA-256 preimage defined in D2 §3.1/§3.3; vectors `libs/jsonschema/runtime/v1/fixtures/fanout-interrupt-key-vectors-v1.json` | `TestFanoutInterruptKeyAndMemberCallIDVectors` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:345`) | pass |
| ACK binding digest is reproducible | `decision_sha256` over canonical JSON (D2 §7) | `TestFanoutDecisionDigestsInFetchFixtures` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:406`) | pass |
| HTTP effect digests are reproducible | render vectors `fixtures/http-action-render-vectors-v1.json` | `TestHTTPActionRenderVectorsAreSelfConsistent` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:454`) | pass |
| Canonical bytes are byte-stable across Go/Rust/Python | reference writer `writeCanonical`/`writeCanonicalString` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:212,257`) | fixture round trip in `TestRuntimeContractSchemasAndFixtures` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:289`) | pass (102/102 fixtures) |

## Resilience

Bounds, typed failures, cancellation, deadline and lease-loss handling: D1 §15 Resilience table (limits module
`fanout/limits.rs` plus `fanout_limits_match_contract_schemas`), D2 §12 Resilience table
(`TestExecutionInterruptLimitsMatchContractSchemas`), D5 §16, D6 §12.

| Rule this PR enforces | Mechanism | Test | Result |
|---|---|---|---|
| Every contract object is closed; every string, array and number is bounded | `lintStrict` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:132`) over every `fanout-*`/`http-action-*` schema | `TestRuntimeContractSchemasAndFixtures` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:289`); mutation: an unbounded property named `default` fails | pass |
| Bounds hold at the edge | invalid fixtures one defect away from a valid one (e.g. `fanout-member-v1.invalid.parallel-ordinal-over-16`, `…map-member-out-of-range`, `fanout-interrupt-list-v1.invalid.seventeen-open`, `fanout-member-status-v1.invalid.end-with-open-cards`) | same test: 46 + 17 invalid fixtures rejected, 28 + 9 valid accepted | pass |
| Malformed fixtures cannot hide | family-prefixed names must match `fixtureName` (`services/elitea-main/internal/runtimecontracts/schemas_test.go:30`) | same test; mutation: misnamed fixtures fail | pass |

## Security

Threat model and per-rule mechanism + test: D1 §15 Security table, D2 §12 Security table (negative-authz,
fencing, consume-once, log-capture tests), D5 §16, D6 §12. Summary of the threats: authorization inside the
effect/decision transaction, identifiers that never authorize, consume-once and multi-tab, rejection/Skip never
permission, egress only through the hardened guard (no redirects, private/CGNAT/metadata/NAT64 blocked, `Proxy=nil`,
HTTPS), credentials by reference only, parameterized database access only, no secrets/prompts/item data/tool
arguments/decision values in events or logs, no arbitrary code outside the Code sandbox.

| Rule this PR enforces | Mechanism | Test | Result |
|---|---|---|---|
| Card display cannot carry secrets | closed `display` in `fanout-interrupt-card.schema.json` | fixture `fanout-interrupt-card-v1.invalid.secret-display-field` rejected | pass |
| Decision bodies carry no tokens; `credential_ref` only for `authorize` | `fanout-interrupt-decision-request.schema.json` | `…decision-request-v1.invalid.raw-token-in-body`, `…authorize-without-credential-ref`, `…credential-ref-on-approve` rejected | pass |
| Resolved frames and fetch entries leak no value or actor | `fanout-interrupt-resolved`, `fanout-interrupt-fetch` schemas | `…resolved-v1.invalid.value-leaked`, `…fetch-v1.invalid.decided-by-leaked` rejected | pass |
| No URL, method or credential in HTTP YAML; no CR/LF header injection | `http-action-node.schema.json` | `http-action-node-v2.invalid.{url,method,credential}-in-yaml`, `…crlf-header-value` rejected | pass |
| Rule origins are https without userinfo or path | `http-action-rule.schema.json` | `http-action-rule-v1.invalid.{http-origin,origin-with-path}` rejected | pass |
| Fixtures contain no real secrets | synthetic values only | security review (below) | pass |

Reviews and scans:
- `code-review` (high): 9 findings; 8 fixed, 1 skipped (adding `libs/jsonschema/**` to `ci-go.yml` — CI changes are
  out of scope by user decision; the gap is recorded above).
- `security-review`: no vulnerabilities found (fixtures synthetic; schemas refuse plain-http origins, userinfo,
  CR/LF headers, raw tokens; the test-only package and docs add no attack surface).
- `govulncheck ./...` in `services/elitea-main` (local go1.26.5): 7 reachable findings, all in the Go standard library
  of the local toolchain (GO-2026-6218, -6091, -6090, -6089, -6088, -5972, -5026; fixed in go1.26.6), all
  pre-existing and none reachable from `internal/runtimecontracts`. New findings from this PR: 0.
- `cargo deny --all-features check advisories`: not run — this PR changes no Rust code or dependency
  (Worker changes are documentation only).
- Supply chain: no new dependency; the Go test uses `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3, already a
  direct dependency of `services/elitea-main`. `golangci-lint` v2.9.0 runs in CI (not installed locally); its first
  run flagged staticcheck QF1001 in the new test (fixed; `staticcheck -checks all` is clean locally).
- CI `Test` job: fails on `TestPublicPinRoutesConcurrentUpsertsKeepOneSharedRow`
  (`services/elitea-main/internal/api/v2/folders`, concurrent pin → HTTP 500). Pre-existing: `main` fails the same
  test on its last two Go CI runs (`604a49e1`, `fcf86c31`); this PR does not touch that package.

## Recovery guarantees (designed; proven in Wave 2)

Answer to "what happens if Main, the Worker or the supervisor goes down?" for the phases these designs touch. Classes
per `rules/replatform-delivery-gate.md` §2. This PR ships no runtime code, so each row cites the design section that
fixes the mechanism, existing code where it already enforces it, and the named Wave 2 proving test.

| Component × phase | Class | Mechanism (design / existing code) | Proving test (Wave 2 unless noted) |
|---|---|---|---|
| Worker × fan-out child, completed | R | child `Completed` receipt short-circuits invocation; frozen child identity (D1 §3, §8) | `cross_execution_restore_zero_reinvocations` (PG) |
| Worker × fan-out child, running | R (child) / I (in-flight model call) | replacement claim restores the child from its last checkpoint; writer fencing `state/postgres_checkpointer.rs:944` | `lease_revoke_mid_child_emits_no_failure`; crash seam `after_child_receipt` |
| Worker × HITL pause and decision | R | card in the Main ledger; decision re-fetched on restore; apply proven on the child checkpoint; ACK after child save (D1 §5.3, D2 §7) | crash seams `after_fetch`, `after_apply`, `before_ack`; `two_claims_apply_same_key_exactly_once` |
| Worker × join | R | `freeze()` returns the stored occurrence; deterministic join from receipts (D1 §8) | crash seam `before_join` |
| Main × decision POST | I | one transaction; client retries with the same `request_id` → byte replay (D2 §6) | `TestDecideFiftyConcurrentTwoTabs`, replay test (Track M2) |
| Main × park / wake | R | continuation + outbox in the decision or settlement transaction; one active continuation per response (D2 §8) | `TestDecideVersusParkExactlyOneContinuation` |
| Main down while children run | R | children keep running; fetch is one bounded attempt and retried on the next tick; frames replay from the output spool (D1 §5.4) | process-loss test: stop Main mid-fan-out, restart, one final answer |
| NATS × wake delivery | I | PostgreSQL outbox is the authority; duplicate wake collapses on the idempotency key (D2 §8) | duplicate-delivery test |
| PostgreSQL unavailable | R | writes fail closed → control stop → replacement claim restores (D1 §6) | `lease_revoke_mid_child_emits_no_failure` |
| Web × reload / second tab | R / I | open cards from `GET …/interrupts`; `agent_hitl_resolved` + 409 quietly consumed (D2 §6, §9) | browser: reload keeps cards; two tabs, one 409 |
| LLM gateway × model call | F → I | until 5b-LLM: typed activation failure; after: child-level retry before first token only, `ModelOutputIncomplete` after it (D1 §6, §9.4) | 5b-LLM PG test (429 → 503 → success, 3 attempts, 0 UI failures) |
| Sandbox supervisor × fan-out Code child | — | not admitted until Wave 3 (Gate 6 receipts, Code-slot semaphore) | `effectful_worker_rejected_at_compile` |
| Main × HTTP action, crash after receipt | R | Lookup returns the receipt; 0 extra requests (D5 §13 case 1) | counting-server process-loss test |
| Main × HTTP action, crash between Begin and Commit | C | `dispatching` → `uncertain/reconciliation_required`, never re-dispatched (D5 §8, §13 case 3) | PG crash-window test |
| Worker × HTTP action, 503 before any effect | I | Lookup finds nothing; Begin proceeds once (D5 §13 case 6) | unit |
| Worker × database read | I | read-only transaction; safe retry (D6 §12) | real-DB retry test (Wave 3) |
| Worker × database write, unknown outcome | C | marker table + session-gone check → committed or `verified_no_effect` (D6 §7, §12) | real-DB kill-mid-write test (Wave 3) |

Pre-existing gap (not widened by these designs): an approved effectful tool that crashes between dispatch and its
tool-result checkpoint can be re-dispatched (root HITL today). It stays closed for fan-out workers until Gate 6 effect
receipts make it class C.

## Security categories (`rules/security.md`)

| Category | Applies to this PR | How checked |
|---|---|---|
| Trust boundaries and identity | Yes (designs) | Private routes require mTLS + claim + fence; identity never from YAML, browser or model (D1 §15, D2 §12). No code here. |
| Authorization (object level) | Yes (designs) | Authorization inside the decision/effect transaction with named negative-authz tests per route (D2 §12, D5 §16, D6 §12); Track M2 must add the full authorization matrix (owner, member, foreign project, other user, unauthenticated, token vs session) for both decision routes. |
| Input, parsing, amplification | Yes | Strict closed schemas with bounds, enforced by `lintStrict` and 63 invalid fixtures; body caps before parsing (D2 §12). |
| Injection and construction | Yes (designs) | HTTP path segment encoding, CR/LF/NUL refusal (schema-enforced for header literals), no templated URLs; DB typed binds only (D5 §6, D6 §5). |
| Egress and SSRF | Yes (designs) | Hardened guard requirements and tests (D5 §10, §16); implemented by Track M1. |
| Secrets | Yes | Diff secret scan: 0 matches; fixtures synthetic; tokens only by reference (`credential_ref`), schema-refused in bodies. |
| Supply chain | Yes | No new dependency; `govulncheck` 0 new findings (7 pre-existing stdlib). |

## D2 decisions confirmed

Confirmed by the user, relayed through the Point 5 planning session, 2026-10-08 (recorded in D2 §13):

1. Coordinator cards keep the interrupt `kind`; "kind empty" means no fan-out metadata (`fanout_v1: null`).
2. The 16-card cap counts `PENDING` + `DECIDED`.
3. The 8 KiB decision cap stands; larger `edit`/`answer` values do not go through the ledger.
4. 409 code `agent_interrupt_already_resolved`.
5. Static pauses use `continue`; member status includes `cancelled`; `interrupt_id` printable ASCII ≤ 512.

## Corrections to the plan found while writing

- `node-recovery-v1.md` is added by #1084; it is not on `main`. D2 cites it as "lands with #1084".
- `agent_hitl_resolved` does not exist anywhere today; D2 introduces it.
- `sibling_ordinal` is parsed as 1..16 today (`src/agents/events.rs:5725-5759`); Map members need 1..64 (Wave 2).
- Shared migration `0154` is taken on `main` (`0154_agent_stop_question_author.sql`); D5 names no number.
- `HTTP_ACTION_INTEGRATION_READY` does not exist yet; Wave 2 adds it.
- Map V1 has a 2 MiB frozen-plan cap (`src/agents/graph/map_reduce.rs:25`) that binds a full 64-item map before the
  per-item cap, and a map cannot coexist with a fixed Parallel node (`map_compiler.rs:124-128`).

## Browser confirmation

Not applicable: this track is documents, schemas and a contract test only (brief T-D "Acceptance").
