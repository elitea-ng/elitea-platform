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
| Schema validation in CI | `services/elitea-main/internal/runtimecontracts/schemas_test.go`; `.github/workflows/ci-go.yml` | Runs in the Go CI job (`go test`). `ci-go.yml` now also triggers on `libs/jsonschema/**`. |

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


## How the fixtures were created

All fixtures are synthetic; none comes from a UI session or a database. The `fanout-*` schemas and fixtures were
written by a one-off Python generator (each invalid fixture is one valid fixture with exactly one defect; digests
computed with `hashlib`), and the `http-action-*` render vectors by a Python reference renderer using the Worker
`encode_component` character set. The generators are not committed: the Go test recomputes every digest
independently, so a hand edit that breaks a fixture fails CI.

## Performance

- **Budgets stated:** fan-out D1 §9 and §15 (≤2 parent rows per activation, 2 transactions to prepare N children, ≤4
  progress frames/s, per-child overhead p99 ≤30 ms, restore of 64 children p95 ≤500 ms, pool wait p99 ≤50 ms, decision
  POST p95 ≤150 ms, live pickup p95 ≤2 s, parked → running p95 ≤5 s, other tab ≤3 s); contract D2 §12 (one
  transaction per decision, 0 fetches with no open card, fetch ≤16 × 64 KiB); HTTP D5 §16 (Main overhead ≤15 ms p95,
  ≤50 ms p95 added to token reads under 16 concurrent actions); DB D6 §12 (*proposal* ≤15 ms p95 Main overhead).
- **Measured:** nothing at runtime — this track ships no runtime code. The Wave 2 tracks measure against these
  budgets. The contract test runs in about 0.2–0.3 s.
- **Result:** not applicable for runtime; budgets recorded for Wave 2.

## Durability

- **Crash windows named:** D1 §8 (12 windows of the fan-out apply path, adjusted to the child-checkpoint apply point),
  D2 §12 (ledger windows: raise, decide, fetch-before-ACK, lost ACK response, stop/regenerate, park vs decide), D5 §13
  (18 HTTP crash cases), D6 §12 (database unknown-outcome windows).
- **Tested now:** the identities that make recovery idempotent are pinned by vectors and recomputed in CI:
  `interrupt_key`, member `call_id`, `decision_sha256`, HTTP body and request digests.
- **Result:** vectors pass. Real-PostgreSQL and process-loss proofs are listed per window for Wave 2 (D1 §13, D5 §15).

## Resilience

- **Bounds stated:** every schema object is closed and every string, array and number bounded — enforced by the
  conformance test's strictness lint over all 20 new schemas. Design bounds: 16 branches, 64 items, ≤8 running, 16
  open cards, 512 KiB item, 8 MiB joined, 8 KiB decision, 32 KiB card, 64 KiB fetch entry, 64 child permits, 16
  streams per model, HTTP 256 KiB inputs / 384 KiB request / 2 MiB response / 30 s, DB 1,000 rows / 512 KiB / 30 s.
- **Typed failures and control stops:** D1 §6 and §15, D2 §6 errors, D5 §16, D6 §12.
- **Tested now:** 46 + 17 invalid fixtures (unknown keys, out-of-range values, wrong combinations, raw tokens, URL in
  YAML, plain-http origins) are rejected by the schemas in Go and in Python.
- **Result:** pass.

## Security

- **Threats addressed in the designs** (D1 §15, D2 §12, D5 §16, D6 §12): authorization inside the effect/decision
  transaction for the exact actor, project and target; identifiers from YAML, browser or model never authorize;
  consume-once and multi-tab safety (CAS, `request_id` replay, 409, ACK after child save); rejection and auth Skip
  never become permission; egress only through the hardened guard (no redirects, private/CGNAT/metadata/NAT64 ranges
  blocked, `Proxy=nil`, HTTPS with verified TLS); credentials by reference only (`credential_ref`,
  `configuration_id`), never in YAML, bodies, events, checkpoints or logs; parameterized database access only, frozen
  statements, read-only transactions for reads; no prompts, item data, tool arguments, decision values or URLs in
  events or logs; no arbitrary code outside the Code sandbox.
- **Enforced now by schemas:** card `display` is closed (no `www_authenticate`, tokens or checkpoint ids); decision
  bodies refuse unknown fields such as a raw token; `credential_ref` is required for `authorize` and forbidden
  otherwise; HTTP node YAML refuses URL, method and credential keys; rule origins must be `https://` without a path;
  fetch entries cannot carry `decided_by`.
- **Evidence redaction:** fixtures use synthetic ids only; no real tokens, URLs with secrets or customer data.
- **Supply chain:** no new dependency. The Go test uses `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3, already a
  direct dependency of `services/elitea-main` (`go.mod`). `govulncheck` is not installed locally and no CI workflow
  runs it, so it was not run; this PR adds no dependency or production code, so it cannot change its findings.
  `golangci-lint` v2.9.0 runs in CI (not installed locally).

## Decisions in D2 that need the reviewer's confirmation

1. Coordinator cards keep the interrupt `kind`; "kind empty" read as `fanout_v1: null`.
2. Open-card cap counts `PENDING` + `DECIDED` (exact form of "16 PENDING").
3. 8 KiB decision bound vs today's 256 KiB `edit`/`answer` values.
4. 409 code spelled `agent_interrupt_already_resolved`.
5. New action `continue` for static pauses; new member status `cancelled`.

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
