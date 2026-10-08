# Fan-out interrupt decisions v1

Status: contract draft for review, 2026-10-08. Wave 1 (Track D2). Nothing in this contract is wired yet.
Producers and consumers: Worker (Wave 2 `FanoutRunner`), Main (Track M2 ledger and API, Wave 2 wiring), Web
(Wave 2). Runtime design: `services/elitea-worker-rust/docs/fanout-v2-runtime-design.md`.

The versioned JSON schemas under `libs/jsonschema/runtime/v1/fanout-*.schema.json` own every HTTP/JSON and frame
metadata shape in this document. Where this text and a schema disagree, the schema wins and this text is a bug.
Canonical fixtures live in `libs/jsonschema/runtime/v1/fixtures/fanout-*`. The Go test
`services/elitea-main/internal/runtimecontracts/schemas_test.go` validates them in CI (see "Conformance").

## 1. Scope

This contract replaces the complete-decision-set resume for every human interrupt. One card is one interrupt. Each
card is decided, delivered, applied and consumed on its own. Siblings keep running. Nothing waits for a complete set.

It covers four interrupt kinds:

| `kind` | Today's guardrail (`display.guardrail_type`) | Actions |
|---|---|---|
| `tool_guard` | `sensitive_tool` | `approve`, `reject`, `edit`, `block_with_comment` |
| `hitl_node` | `pipeline_hitl` (HITL node), `pipeline_static` (static pause) | `approve`, `reject`, `edit`; `continue` for a static pause |
| `ask_user` | `clarifying_question` | `answer` |
| `delegated_auth` | `mcp_auth` | `authorize`, `skip` |

The action vocabulary is today's (`services/elitea-main/internal/application/agentexecution/continue.go:926-937`;
`services/elitea-worker-rust/src/agents/direct_hitl.rs:132-142`). `continue` is new: a static pause today resumes
through a typed chat message (`src/agents/graph/static_pause.rs:24,228-250`). In the ledger the same text is the
decision `value`.

It covers two scopes:
- **Fan-out members.** A Parallel branch or a Map item, at any depth inside that member.
- **Coordinator (root) pauses.** User decision (PLAN §6 answer 2): root, non-fan-out pauses move to the same ledger in
  Wave 2, right after fan-out. Coordinator cards use the same schemas with an **empty hierarchy**
  (`parent_agent_name: null`, `parent_agent_call_id: null`, `parent_agent_path: []`) and `fanout_v1: null`. The
  interrupt `kind` is always set: the ledger needs it, and the brief's "kind set to empty" is read as the fan-out
  `fanout_v1.kind`. Reviewers: confirm this reading.

Out of scope: progress coalescing and scheduling (runtime design), node recovery (`node-recovery-v1.md`, lands with
#1084), effect receipts (Gate 6).

## 2. Schemas

All `$id`s are `elitea.pipeline.<stem>.v1`. Cross-schema `$ref` uses the `$id`.

| Schema stem | Carried in | Producer → consumer |
|---|---|---|
| `fanout-member` | `fanout_v1` object inside cards and frames | Worker → Main, Web |
| `fanout-hierarchy` | the existing `parent_agent_*` fields (definition source) | Worker → Main, Web |
| `fanout-interrupt-card` | `agent_interrupt_pending` frame, `response_metadata.interrupt_card_v1`; ledger `display`; list API | Worker → Main → Web |
| `fanout-interrupt-list` | `GET …/interrupts` response | Main → Web |
| `fanout-interrupt-decision-request` | `POST …/interrupts/{interruptKey}/decision` body | Web → Main |
| `fanout-interrupt-decision-result` | 200 body of the decision POST | Main → Web |
| `fanout-interrupt-error` | 400 / 404 / 409 body of the decision API | Main → Web |
| `fanout-interrupt-fetch` | private fetch response | Main → Worker |
| `fanout-interrupt-ack-request` | private ACK body | Worker → Main |
| `fanout-interrupt-ack-result` | private ACK response | Main → Worker |
| `fanout-interrupt-resolved` | `agent_hitl_resolved` frame, `response_metadata.interrupt_resolved_v1` | Main → Web |
| `fanout-member-status` | `agent_fanout_member` frame, `response_metadata.fanout_member_status_v1` | Worker → Main → Web |
| `fanout-parked` | terminal frame of a parked execution, `response_metadata.fanout_parked_v1` | Worker → Main → Web |
| `fanout-wake` | `payload.meta.fanout_wake_v1` of a wake continuation | Main → Worker |
| `fanout-interrupt-key-vectors` | test vectors only | — |

Common grammar:
- Digests and request ids are lowercase 64-hex, never all zeros.
- `execution_id` is 32 lowercase hex (Main `currentRuntimeID`). `response_message_id` is a nonzero 36-character UUID.
  They are never interchangeable (same rule as node recovery).
- Revisions are integers `1..9223372036854775807`. `decision_revision` may be 0 (nothing decided yet).
- Free text has no C0/C1 controls, U+2028 or U+2029. Decision `value` allows tab, LF and CR.
- Unknown keys, wrong types and out-of-range values fail. Every object is closed and every string, array and number
  is bounded (enforced mechanically by the conformance test).

## 3. Identity

### 3.1 `interrupt_key` (Worker-minted, opaque to Main)

```
interrupt_key = lowercase_hex(SHA-256(
  "elitea.graph.fanout-interrupt.v1" 0x00
  || LP(root_thread) || LP(fanout_node_id) || U64(step) || LP(config_digest) || U64(ordinal)
  || LP(child_thread) || LP(child_paused_checkpoint_id) || LP(interrupt_id) || LP(tool_call_id)))
LP(s)  = u32 big-endian byte length of UTF-8 s, then the bytes
U64(n) = u64 big-endian
```

- `root_thread`: the conversation's root graph thread. `fanout_node_id`, `step`, `config_digest`: the fan-out node,
  its super-step and its compiled config digest. `ordinal`: 1-based member ordinal.
- `child_thread`: the **frozen** child thread (Track C1: identity is frozen at occurrence creation and never derived
  from `execution_id`, generation or claim). `child_paused_checkpoint_id`: the child checkpoint that holds the pause.
- `interrupt_id`, `tool_call_id`: the existing per-interrupt identity; `tool_call_id` is `""` when there is none.
- **Coordinator cards** use the same function with `fanout_node_id = ""`, `step = 0`, `config_digest = ""`,
  `ordinal = 0`, `child_thread = root_thread` and the root's paused checkpoint id.

A new pause of the same child (a later checkpoint) is a new key. Rewind or regenerate creates a new occurrence, new
children and therefore new keys. A worker restart or a continuation execution re-derives the same key, so a decision
survives both. Main never parses or recomputes the key. Vectors: `fanout-interrupt-key-vectors-v1.json` (includes a
Map ordinal 64 case with non-ASCII ids and the coordinator case).

### 3.2 `interrupt_id`

The existing Worker interrupt id stays on the card because Web merges and keys cards by it
(`apps/elitea-web`, Track W). The Worker must keep it unique within one root response. Main refuses a raise whose
`interrupt_id` equals another open card's id under a different key (typed fault, no row).

### 3.3 Member hierarchy tier (user decision, PLAN §6 UI correction)

Fan-out members render exactly like today's EliteaUI sub-agent instances. No new visual container exists.
- Each member adds **one** tier to the existing `parent_agent_path` (`src/agents/events.rs:5761-5769` shape
  `{name, call_id, sibling_ordinal}`). Enclosing tiers (for example an agent that owns the pipeline) come first.
- `name`: the worker's display name (saved agent name for an Agent worker; node id for LLM and Pipeline workers).
- `call_id = "fo1_" + lowercase_hex(SHA-256("elitea.graph.fanout-member-call.v1" 0x00 || LP(child_thread)))`. It is
  stable across restarts and continuations because `child_thread` is frozen. Vectors in the same fixture.
- `sibling_ordinal`: the 1-based member ordinal, the backend-authoritative ordinal used by `computeBreadcrumbs`.
- Labels therefore come out of the existing breadcrumb contract unchanged: three branches of `Researcher` that call
  `Sub` render `Researcher (1) ▸ Sub`, `Researcher (2) ▸ Sub`; distinct branch names are not numbered; leaf tiers are
  never numbered. Cards bucket by `parent_agent_call_id` as today
  (`EliteaUI/src/[fsd]/features/chat/lib/hooks/useApplicationAnswerState.hooks.js:292-349`).
- Bounds that change in Wave 2: `parse_agent_path_tier` accepts `sibling_ordinal` 1..16 today (`events.rs:5725-5759`,
  `MAX_TOOL_CALLS_PER_MODEL_TURN` `events.rs:50`). The member tier needs 1..64 (Map). The schema allows 1..64 on every
  tier; the Worker widens its parser for fan-out tiers. The member tier counts toward `MAX_AGENT_PATH_TIERS = 3`
  (`events.rs:108`), so the compiler must reject a fan-out worker whose nested agent depth would exceed it.

### 3.4 `fanout_v1` (additional metadata only)

`{kind: parallel|map, node, activation, member, ordinal, total}`. It feeds grouping counters and the Map status
buckets for more than 16 items (PLAN §6 answer 5). It never creates a container and carries no item content.
- `activation`: 16 lowercase hex, the first 64 bits of
  `SHA-256("elitea.graph.fanout-activation-label.v1" 0x00 || LP(private activation identity))`.
- `member`: the Parallel branch id, or the decimal 0-based Map item index.
- `ordinal`: 1-based, equal to the member tier's `sibling_ordinal`. `total`: member count.
- Parallel: `ordinal`, `total` ≤ 16. Map: ≤ 64.

## 4. Interrupt card (raise)

The Worker raises a card in a **non-terminal** `agent_interrupt_pending` frame as soon as a child pauses. Fields
(schema `fanout-interrupt-card`): `interrupt_key`, `interrupt_id`, `kind`, `available_actions` (1–4, unique, legal for
the kind), `payload_sha256` (digest of the Worker's full private pending object), `display`, the three hierarchy
fields and `fanout_v1`.

`display` is closed and holds only fields the current card shows: `message` (≤8 KiB), `guardrail_type`, `node_name`,
`tool_name`, `toolkit_name`, `toolkit_type`, `action_label`, `tool_call_id`, `tool_args_json` (≤16 KiB, the canonical
JSON text the card shows), `policy_message`, `interaction_type`, `edit_state_key`, and `server_url` (https only) for
auth. Headers such as `www_authenticate`, tokens, resource metadata and checkpoint ids are never in a card. The
canonical card is at most 32 KiB; Main enforces bytes.

Coherence the schema enforces: hierarchy is null/null/[] or name/call_id/≥1 tier; a card with `fanout_v1` has at
least one tier; `available_actions` and `display.guardrail_type` match `kind`.

## 5. Ledger (Main)

Track M2 builds this; Wave 2 wires it.

```
elitea_runtime.execution_interrupts
  PRIMARY KEY (root_response_id, interrupt_key)
  project_id, conversation_id, raising execution_id + generation, kind, available_actions,
  frontier JSONB (private: child thread, ordinal, fan-out node; never returned to a client),
  card_json (canonical card bytes, ≤32768), payload_sha256,
  state  PENDING | DECIDED | CONSUMED | CANCELLED | SUPERSEDED,
  revision BIGINT, request_id, decision_json (canonical decision body, ≤8192, no secrets),
  decided_by, decided_at, consumed_claim_id, consumed_checkpoint_id, consumed_at, closed_at,
  source_event_id UNIQUE
elitea_runtime.execution_interrupt_audit
  (root_response_id, interrupt_key, transition RAISED|DECIDED|CONSUMED|CANCELLED|SUPERSEDED,
   actor_id or claim_id, at)
per-response decision_revision BIGINT (side table or existing response-level row)
```

| From | Event | To | Revision |
|---|---|---|---|
| — | accepted `agent_interrupt_pending` | `PENDING` | 1 |
| `PENDING` | decision POST (CAS on `expected_revision`) | `DECIDED` | +1; response `decision_revision` +1 |
| `DECIDED` | ACK `applied` | `CONSUMED` | +1 |
| `DECIDED` | ACK `stale` | `SUPERSEDED` | +1 |
| `PENDING`, `DECIDED` | stop, cancel, fail-after-drain | `CANCELLED` | +1 |
| `PENDING`, `DECIDED` | regenerate, rewind, new turn | `SUPERSEDED` | +1 |

CHECK coherence, modelled on `services/elitea-main/migrations/shared/0146_node_recovery_control.sql:56-57`:
- `PENDING`: decision, consumption and `closed_at` columns are all NULL; `revision = 1`.
- `DECIDED`: `request_id`, `decision_json`, `decided_by`, `decided_at` NOT NULL; consumption columns NULL.
- `CONSUMED`: decision columns and `consumed_claim_id`, `consumed_checkpoint_id`, `consumed_at` NOT NULL.
- `CANCELLED`, `SUPERSEDED`: `closed_at` NOT NULL; consumption columns NULL; decision columns either all NULL or all
  NOT NULL.

Rules:
- **Raise is idempotent** on `(root_response_id, interrupt_key)`. A byte-identical replay is a no-op. Different bytes
  under the same key are a typed fault (no update).
- **Open cap.** At most 16 open cards (`PENDING` or `DECIDED`) per root response. This is exact: the Worker counts a
  card as open until it is consumed and stops admitting work at 16 (same bound as `MAX_PAUSE_CARDS`,
  `src/agents/graph/parallel.rs:38`, and `maxCurrentHITLDecisions`, `continue.go:22`). A raise beyond the cap is a
  typed fault. The binding plan says "16 PENDING"; counting `DECIDED` too is the stricter, exact form. Reviewers:
  confirm.
- **Secrets.** `decision_json` holds the canonical decision body. Delegated auth stores a token-store reference
  (`credential_ref`), never a token. ASSUMPTION: the existing MCP token store can return a short opaque reference;
  Wave 2 confirms its API.
- Cancellation and supersede run in the same transaction as the stop/regenerate (pattern
  `services/elitea-main/internal/db/queries/agent_cancel.sql:53-55`, the `cancelled` CTE). A dead fence then blocks any late ACK.

## 6. Public decision API

```
GET  /api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}/interrupts
POST /api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}/interrupts/{interruptKey}/decision
```

Same prefix and RBAC as node recovery: GET uses `models.applications.task.get`, POST uses
`models.chat.messages.create`. Main rechecks the original actor's conversation ownership and membership, the active
user and project, **inside** the transaction. Wave 1 registers the handlers behind a Main flag that is off in
production (Track M2).

**GET** returns schema `fanout-interrupt-list`: the open cards (`PENDING`, `DECIDED`) with state and revision, and the
response's `decision_revision`. Web uses it on reload and to reconcile.

**POST** body is `fanout-interrupt-decision-request`: `{request_id, expected_revision, action, value[, credential_ref]}`.
- The canonical body is at most 8192 bytes (Main checks bytes; `value` is also capped at 8192 characters).
- `approve`, `reject`, `authorize`, `skip`: `value` is `""`. `edit`, `block_with_comment`, `answer`, `continue`:
  `value` is non-empty. `credential_ref` is required for `authorize` and forbidden otherwise.
- `action` must be in the card's `available_actions`.
- `expected_revision` is the row revision the client saw. A `PENDING` row always has revision 1 in v1.

Transaction: lock the root response row, then the card row; recheck RBAC; then:

| Row state | Request | Result |
|---|---|---|
| `PENDING`, revision matches | — | `DECIDED`, revision+1, `decision_revision`+1, audit `DECIDED`; 200 `replay:false` |
| any decided state | same `request_id`, byte-identical canonical body | 200 `replay:true` with the current state (`DECIDED` or `CONSUMED`) |
| anything else | — | 409 `agent_interrupt_already_resolved` |
| no such card for this response | — | 404 `agent_interrupt_not_found` |
| body or action invalid | — | 400 `agent_interrupt_invalid_decision` |

Error bodies follow `fanout-interrupt-error` (`{error, message, retryable:false}`), the same envelope family as the
existing `agent_hitl_already_resolved` (`services/elitea-main/internal/api/v2/agentexecution/route.go:603-620`). The
brief's name `already_resolved` is spelled `agent_interrupt_already_resolved` so Web's existing `*_already_resolved`
handling applies. Web treats 409 as "quietly consumed": it drops the card, shows no error and never falls back to the
socket.

After commit Main emits `agent_hitl_resolved` (section 9), so other tabs drop the card. If the execution is parked,
the same transaction admits the wake continuation (section 8).

**Tightening to review:** today an `edit`/`answer` value may be 256 KiB (`maxCurrentHITLValueBytes`, `continue.go:21`).
The binding 8 KiB decision bound makes larger edits impossible in the ledger. Wave 2 must either accept that or carry
large values as a content reference.

## 7. Private Worker routes

Same transport as node recovery control (`libs/proto/contracts/node-recovery-v1.md`, lands with #1084): the private
mTLS HTTPS content listener, the verified workload certificate, `X-Elitea-Claim-Id` and `X-Elitea-Fence` (32 bytes,
base64 RawURL, no padding). Main rechecks the current live claim and session, the immutable manifest, the original
actor's permissions, the deadline and "no terminal result" under the job lock, and repeats the authority lookup with
the DB clock after taking the lock. The claim must be an ordinary `RUNNING` claim; a `NODE_RECOVERY` claim is refused.
One bounded attempt per call, no redirects, no automatic network retry.

```
POST /executions/{executionID}/generations/{generation}/interrupts/fetch   (empty body)
POST /executions/{executionID}/generations/{generation}/interrupts/ack     (fanout-interrupt-ack-request)
```

**Fetch** returns `fanout-interrupt-fetch`: every `DECIDED` row of the execution's root response, at most 16, each
canonical entry at most 64 KiB, plus `decision_revision`. It never returns `decided_by` or the private frontier.
Each entry carries `decision_sha256`:

```
decision_sha256 = lowercase_hex(SHA-256(canonical_json({
  action, credential_ref (null when absent), interrupt_key, request_id, revision, value })))
```

Canonical JSON (shared with `services/elitea-worker-rust/docs/http-action-node-design.md` §6): object keys sorted by
UTF-8 bytes at every level, compact, exact integers, strings escaped only for `"`, `\` and characters below U+0020
(short forms `\b \t \n \f \r`, otherwise `\u00xx` lowercase), every other character raw UTF-8 (including U+2028,
U+2029 and `<>&`), no trailing newline. Rust `serde_json` and Python `json.dumps(sort_keys=True,
separators=(",",":"), ensure_ascii=False)` produce it directly. Go `encoding/json` does not (it always escapes U+2028
and U+2029), so Main uses a small canonical writer; the conformance test contains a reference one. Fixtures:
`fanout-interrupt-fetch-v1.json` (the Go test recomputes the digests).

When the Worker fetches:
- **Live:** only while the activation has at least one open card, every 1 s, backing off to 5 s while fetches return
  nothing new; reset to 1 s when a new card opens. Zero fetches while no card is open.
- **Restore:** once, immediately, whenever a fan-out (or coordinator pause) restores, including in a wake continuation.
- Before parking: one final fetch (section 8).

**Apply rule (Worker).** For each fetched decision the Worker loads the child's latest checkpoint and applies the
decision only if it is `Paused` at exactly the card's `child_paused_checkpoint_id` with that `interrupt_id`. It then
re-invokes the child with resume controls. When the child's next checkpoint is saved it ACKs `applied` with that
`child_checkpoint_id`. If the child's latest checkpoint has already moved past the pause (the decision was applied
before a crash) it ACKs `applied` with the current checkpoint id; no second resume happens. If the card can no longer
apply (wrong pause, child completed, occurrence superseded) it ACKs `stale`. A decision for a child that is running
(a multi-card Agent child between pauses) is held until that child pauses again or finishes.

**ACK** body: `{interrupt_key, request_id, revision, decision_sha256, outcome, child_checkpoint_id}`; `applied` needs
a checkpoint id, `stale` has `null`. `revision` and `decision_sha256` must equal the fetched values. Result
`fanout-interrupt-ack-result`: `applied` → `CONSUMED`, `stale` → `SUPERSEDED`, revision+1, audit row with the claim.
Main stores the exact canonical ACK bytes; a byte-identical replay under a current fence returns `replay:true`;
a conflicting replay is 409.

## 8. Park and wake

The Worker parks the execution as soon as nothing can run: no running child, admission closed or exhausted, at least
one open card, and a final fetch that returned nothing new. The terminal frame carries `fanout_parked_v1`
(`fanout-parked`): the pending `{interrupt_key, interrupt_id}` list and `decision_revision_seen` (the revision of the
last fetch). Web reconciles its open cards against this list and never creates cards from it.

Main closes the park/decide race under the **response row lock** (both the paused-finalize transaction and the
decision transaction take it):
- Park settlement: if `decision_revision > decision_revision_seen` or any card is `DECIDED`, admit one continuation in
  the same transaction; otherwise record the response as parked.
- Decision on a parked response: admit one continuation in the same transaction.
- A partial unique index allows **one active continuation per response**. A decision that arrives while a
  continuation is queued or running is picked up by that continuation's restore fetch.

The continuation is a new continuation kind `fanout_wake` (new value next to `hitl`, `authorization`, `output_limit`,
`static` in `CurrentContinuationKind`, `continue.go:32-38`), idempotency key
`continue-interrupt/{responseMessageID}/{decision_revision}`. Its payload carries **no decisions**, only
`payload.meta.fanout_wake_v1 = {schema, response_message_id, decision_revision}` (`fanout-wake`). The Worker re-enters
the pending fan-out node and fetches.

## 9. Stream frames

| Frame `type` | Terminal | Producer | Metadata key → schema | Delivery |
|---|---|---|---|---|
| `agent_interrupt_pending` | no | Worker | `interrupt_card_v1` → `fanout-interrupt-card` | immediate |
| `agent_hitl_resolved` | no | Main (durable in the execution log) | `interrupt_resolved_v1` → `fanout-interrupt-resolved` | on DECIDED, CANCELLED, SUPERSEDED |
| `agent_fanout_member` | no | Worker | `fanout_member_status_v1` → `fanout-member-status` | immediate |
| existing terminal frame of a parked execution | yes | Worker | `fanout_parked_v1` → `fanout-parked` | once |

- `agent_interrupt_pending` and `agent_fanout_member` are new names; `agent_hitl_resolved` is new too (it does not
  exist in Worker, Main or Web today). Main projects `agent_interrupt_pending` into the ledger in the same transaction
  that accepts the frame (pattern `services/elitea-main/internal/infra/db/repos/node_recovery_projection.go`).
- Member status values: `start` (also sent again when a paused member resumes), `end`, `paused`, `failed`,
  `cancelled`. `cancelled` is added to the brief's four for fail-after-drain and user stop. `open_interrupts` is ≥1
  only for `paused`.
- `agent_hitl_resolved` carries the action but never the value. An answered `ask_user` summary on reload comes from the
  persisted message, as today.
- Every member-scoped frame (tool, LLM, interrupt, progress) carries the three hierarchy fields and `fanout_v1`.
  Progress text frames are coalesced per activation (runtime design); the frames above are never coalesced.
- The legacy aggregate `elitea.graph.parallel-interrupt.v1` interrupt is not produced by V2. Web treats any it still
  sees as reconcile-only (Track W).

## 10. Rollout and compatibility

- All admission gates stay `false` (`FIXED_PARALLEL_INTEGRATION_READY`, `MAP_INTEGRATION_READY`, Web
  `CompilerAdmittedNodeTypes`). The Main API flag is off in production.
- Until Wave 2 moves root pauses, the existing root path (`meta.hitl_interrupts`, `ResumeCurrentAgentHITL`, complete
  set) is unchanged. When it moves, the complete-set checks (`continue.go:409-432`,
  `internal/db/queries/agent_chat.sql:2145,2153,2368,2380`) are removed in the same change; there is no period with two
  authorities for one response.
- Deploy Main before Worker. An old Worker never raises `agent_interrupt_pending`, so the ledger stays empty for its
  executions.

## 11. Conformance

`services/elitea-main/internal/runtimecontracts/schemas_test.go` runs in the Go CI job (`go test`). For every
`fanout-*` and `http-action-*` schema it:
1. compiles the schema (Draft 2020-12, `$ref` by `$id`) and checks `$id = elitea.<area>.<stem>.v<N>`;
2. lints strictness: closed objects, `maxLength` on strings, `maxItems` on arrays, `minimum`/`maximum` on numbers;
3. requires at least one valid and two invalid fixtures; valid fixtures must pass and invalid ones must fail;
4. requires every fixture to be byte-canonical JSON in the form defined in section 7 (round trip);
5. recomputes `interrupt_key`, member `call_id` and `decision_sha256` from the vectors.

Fixture names: `<stem>-v<N>[.<variant>].json` (valid) and `<stem>-v<N>.invalid.<reason>.json` (one defect each).
Wave 2 Worker and Main tests must load the same fixtures instead of copying them.

## 12. Performance, durability, resilience and security

### Performance

| Budget | Limit |
|---|---|
| Decision POST | p95 ≤ 150 ms: one transaction (response row lock, card row CAS, audit insert, revision bump, and the wake continuation plus outbox row only when parked) |
| GET list | ≤ 16 cards × ≤ 32 KiB |
| Private fetch | 0 calls while no card is open; ≤ 1/s otherwise (5 s at idle backoff); ≤ 16 entries × ≤ 64 KiB |
| Frames per card | 1 `agent_interrupt_pending` + 1 `agent_hitl_resolved`; lifecycle frames are never coalesced but are O(members), not O(tokens) |
| Ledger rows | 1 row per card + 1 audit row per transition; 0 parent checkpoint rows per card |
| Live pickup / parked wake / other tab | p95 ≤ 2 s / ≤ 5 s / ≤ 3 s |

### Durability

| Window | Durable state | Recovery rule |
|---|---|---|
| Card frame accepted, ledger insert | Same transaction as frame acceptance | A replayed frame is a byte-identical no-op; different bytes under the key are a typed fault |
| Decision committed | Row `DECIDED`, revision and (if parked) wake continuation + outbox in one transaction | Delivered by the next live fetch, the restore fetch, or the wake continuation |
| Fetched, Worker died before apply or ACK | Row `DECIDED` | Refetched under the new claim; the child checkpoint decides apply vs already-applied (runtime design §8) |
| ACK sent, response lost | Row `CONSUMED`/`SUPERSEDED` with exact ACK bytes | Byte-identical replay returns `replay:true`; a conflicting replay is 409 |
| Stop, regenerate, new turn | Open rows → `CANCELLED`/`SUPERSEDED` in the stop transaction | The dead fence blocks a late ACK |
| Park vs decide race | Response row lock + `decision_revision` + one-active-continuation index | Exactly one continuation |

Rows are keyed by the root response, so they survive continuation executions and Worker replacement.

### Resilience

- Bounds: 16 open cards per response; decision body ≤ 8 KiB; card ≤ 32 KiB; fetch entry ≤ 64 KiB; ≤ 16 entries per
  fetch; hierarchy ≤ 3 tiers; ordinal ≤ 64; every string, array and number in every schema is bounded and every object
  is closed (§11).
- Private calls are one bounded attempt, no redirects, no automatic retry. The public API returns typed errors
  (400/404/409) and never a partial state.
- The Main API is behind a flag that is off in production until Wave 2 acceptance.

### Security

- **Authority inside the transaction.** The POST locks the response and card rows, then rechecks the original actor's
  ownership and membership, the active user and project, and RBAC (`models.chat.messages.create`) before the CAS. GET
  uses `models.applications.task.get`. The route's `responseMessageID` selects the response only after
  authorization; `interruptKey` selects a card inside that response and grants nothing by itself. Fail closed.
- **Private routes.** mTLS workload certificate, claim id and fence; `RUNNING` claims only; authority repeated with the
  DB clock under the job lock; the private `frontier` and `decided_by` never leave Main.
- **Consume-once.** CAS on `expected_revision`, `request_id` replay with byte equality, 409 otherwise; ACK binds
  `request_id`, `revision` and `decision_sha256`.
- **No secrets.** Decision bodies never carry tokens (`credential_ref` only, resolved server-side for the original
  actor); `agent_hitl_resolved` never carries the value; logs and audit rows carry keys, states, actor/claim ids and
  digests, never values, display text or tool arguments. Nothing sensitive goes into URLs: path parameters are opaque
  ids and digests, and there are no query strings.
- **Rejection is never permission.** `reject` and auth `skip` are recorded and applied as such.
- **Supply chain.** The conformance test uses `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3, already a direct
  dependency of `services/elitea-main`. No new dependency.

## 13. Open points for the reviewer

1. Coordinator cards keep the interrupt `kind`; "kind empty" is read as `fanout_v1` null (section 1).
2. The open-card cap counts `PENDING` and `DECIDED` (section 5).
3. The 8 KiB decision bound vs today's 256 KiB edit values (section 6).
4. `agent_interrupt_already_resolved` instead of the bare `already_resolved` (section 6).
