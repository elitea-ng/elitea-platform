# Node recovery control v1

This contract preserves one node visit in one execution generation. The existing
accepted NodeEvent transport owns suspension; the private mTLS HTTPS content
listener owns recovery control. It adds no gRPC service. The versioned JSON
schemas under `libs/jsonschema/runtime/v1/node-recovery-*.schema.json` own the
HTTP/JSON shapes; generated protobufs own the claim and desired-state enums.

## Suspension and canonical receipt

Worker emits a nonterminal `agent_node_recovery_required` NodeEvent with
`response_metadata.node_recovery_required_v1`. Main atomically commits the
accepted event, exact canonical receipt and `SUSPENDED` desired state. The job
remains `RUNNING`, the existing response remains streaming, and no terminal
result, new execution, generation or turn is created. Main also stores the receipt
in the original response's `meta.node_recovery_required_v1` for readback.
The complete accepted frame fence binds it. The receipt never grants checkpoint
access and contains no fence token.

Receipt schema is `elitea.pipeline.node-recovery-required.v1`. It has exactly
`schema`, `activation_id`, `journal_revision`, `graph_thread`, `node_id`, `step`,
`attempt`, `failure_class`, `stop_reason`, `replay_safety`, `allowed_actions`.
Identities are lowercase, nonzero 64-character hex. Revisions are positive,
steps unsigned including zero, integers at most 9223372036854775807, attempts
1..16, node IDs 1..128 ASCII `[A-Za-z0-9_.:-]`, graph threads 1..512 UTF-8 bytes
without Unicode controls, U+2028 or U+2029. Unknown keys, duplicates, nulls,
trailing data and terminal failure/stop reasons fail.

`replay_safety` has `kind` and only its required identity:
`no_external_effect` and `unclassified` have no other fields;
`idempotent_effect_not_committed` and `unknown_external_effect` have `effect_id`;
`completed_external_effect` has `receipt_id`.
Eligible typed transient repeatable failures use `operator_approval_required`
and `["retry"]`. Unknown/unclassified effects use `effect_reconciliation_required`
and `["reconcile"]`; completed effects use that reason and `["resume_result"]`.
Authentication/authorization denial, sensitive rejection, cancellation, lease
loss and unknown failures remain terminal. Worker preserves and rechecks the
immutable policy, original backoff, attempt/elapsed budget and complete history.

`receipt_sha256` hashes canonical receipt JSON: lexicographically sorted object
keys at every level, compact UTF-8, exact integers, no HTML escaping and no
trailing newline. Go uses UseNumber/SetEscapeHTML(false); Rust uses sorted
serde_json::Value objects. The excluded thread characters avoid encoder drift.
The 392-byte no-newline fixture has both file and canonical SHA256
`8f592df47f70a2b2f5c4bf0bd97832856cd194d139a0eeab7dc37a82ca4b1351`.

## Execution and response identifiers

Execution selectors are exactly 32 lowercase hexadecimal characters, matching
Main currentRuntimeID (16 random bytes encoded with hex.EncodeToString).
Zero bytes are syntactically valid; this grammar grants no authority.
ResponseMessageID remains a separate nonzero 36-character chat UUID.
Main62 UUID-only execution validation and schemas were an adoption blocker;
this pre-adoption v1 correction supersedes those pins without adding fields.

## Public operator control

GET `/api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}/node_recovery`
uses `models.applications.task.get`; POST at the same path plus `/actions` uses
`models.chat.messages.create`. Existing authentication and project RBAC apply.
Main rechecks the original job actor, conversation ownership, active user/project
and current membership inside the transaction. Stored response binding resolves
the execution scope. Client execution/generation/activation/revision are stale
selectors only. The body has exactly `request_id`, `execution_id`, `generation`,
`activation_id`, `expected_revision`, `action` (`retry|reconcile|resume_result`).
Browser proofs, replacement results, child selectors, credentials and checkpoints
are prohibited. The typed State and Accepted schemas define responses. UI refuses
integers above its exact safe range.

202 follows the serializable transaction committing action/outbox/audit together.
Exact nonce replay returns the same action without dispatch; conflicting reuse,
stale scope and missing owner proof fail with 409. Malformed bodies fail with 400;
absent/inaccessible reads fail with 404. Reattach the existing execution stream.
No action writes the ordinary command outbox or mints an effect.

## Claim and private control

`SUSPENDED = 4`, `RECOVER_NODE_VISIT = 12`, opt-in
`ClaimCommandRequestV1.node_recovery = 17`, and claim receipt field16 carries the
stored receipt bytes. The fresh claim fence/epoch may change on replacement;
execution ID, generation, activation and original actor never change. Claim
receipt retains the original immutable input bundle and DB claim-start timestamp.
Suspended content access permits raw original `agent.execution_request` inspection
only; materialization/credential redemption is skipped. Ordinary BeginExecution
and AuthorizeInvocation reject `NODE_RECOVERY`, including after ACK. A sealed
recovery checkpoint restore is the sole graph resumption authority.

Node-only requests first load the latest node visit across every stored status.
Latest SUSPENDED or AUTHORIZED with desired SUSPENDED selects exact node recovery.
Latest STOPPED with desired RUNNING selects terminal-only node recovery.
No visit or latest RESUMED permits existing model checkpoint inspection only for
Agent RUNNING, desired RUNNING, and original invocation MAY_HAVE_STARTED.
Inspection validates the current root frontier and durable journal before restoration.
It supplies no historical node receipt, fresh Begin, or fresh invocation permission.
This covers lost ACK at pending A and later pending B before Main receives B's receipt.

The same live claim may promote checkpoint inspection to node recovery after
the exact latest SUSPENDED receipt is committed. Mode changes retain the full
current fence and existing one-use model checkpoint digest.
Inspection uses the positive remaining lease bounded by the selected TTL.
It does not extend the lease or replace the DB observation time.
Different checkpoint digests on the same claim remain rejected.
The Worker closes that inspection without Redis ACK and waits for replacement authority.
Private node control and result access require NODE_RECOVERY mode.
An inspection claim cannot replay historical A ACK or result into pending B.

POST `/executions/{executionID}/generations/{generation}/node-recovery/control`
has an empty body. POST the same prefix plus `/ack` has the strict ACK object.
Use the existing verified mTLS workload certificate, X-Elitea-Claim-Id and
X-Elitea-Fence (32 bytes encoded base64.RawURLEncoding, no padding).
Main rechecks current live claim/session, immutable manifest, original actor
permissions, deadline, no terminal result and exact stored visit under the job
lock; it repeats authority lookup using the DB clock after obtaining that lock.
Polling returns schema `elitea.pipeline.node-recovery-control.v1`, execution_id,
generation, desired_state (`SUSPENDED|RUNNING`), receipt and action (`null` or the
stored server action). Action has request_id, activation_id, expected_revision,
last_attempt, action, receipt_sha256, owner_proof (`null` for no-effect retry).
One bounded client attempt; no redirects or automatic network retry.

Worker must durably CAS the exact original journal before ACK. ACK has required
request_id, activation_id, expected_revision, receipt_sha256, applied_revision,
continuation_receipt and terminal_stop_reason. applied_revision=expected_revision+1.
The last two fields are required nullable fields, with exclusive outcomes:

- null/null: retry or verified committed result; consume action, mark RESUMED,
  set desired RUNNING and return sealed checkpoint restore authorization.
- receipt/null: reconcile with verified_no_effect only; preserve failed
  attempt/node/thread/step/class/effect, append exact new receipt at applied
  revision, remain SUSPENDED and require another explicit retry. No auto work.
- null/reason: after actual stopped journal CAS; reasons attempts_exhausted,
  elapsed_limit, not_retryable, retry_disabled. Mark STOPPED, return terminal-only
  settlement authorization and set RUNNING solely for the existing FAILED
  settlement path. No checkpoint restoration or new node attempt.

Main resolves the latest visit from all statuses under the same locked job
before any ACK historical action lookup. Its activation and journal revision
must match the ACK, including replay. A superseded A visit can never receive
fresh authority after B suspends or resumes.
Main persists exact canonical ACK bytes and rejects conflicting applied replays.
A fresh current claim may replay the same action/proof under its new fence; the
returned authorization binds that current claim and cannot be reused by the old
writer. Cancellation atomically cancels pending visits/audits and blocks every
poll/ACK/result read. No cancellation/denial becomes retry.

ACK response schema `elitea.pipeline.node-recovery-ack.v1` has execution_id,
generation, request_id, applied_revision, replay, recovery_resume_authorized,
resumption, terminal_settlement_authorized, terminal_authorization. Resumption is
null or schema `elitea.pipeline.node-recovery-resumption.v1`, execution_id,
generation, request_id, activation_id, journal_revision, input_bundle_id,
input_manifest_sha256, receipt_sha256, claim_id. Terminal authorization is null or
schema `elitea.pipeline.node-recovery-terminal-settlement.v1`, execution_id,
generation, request_id, activation_id, journal_revision, receipt_sha256, claim_id,
stop_reason. The two authorizations are mutually exclusive; evidence-only ACK
returns both false/null. Worker seals and consumes them only against its exact
applied journal and frozen graph checkpoint. Post-ACK frozen runtime-context and
sandbox reads can use RUNNING/current fence; fresh root Begin/Invoke stays denied.

## Owning effect proof and result redemption

Main rejects effect recovery without an actual transactional owner proof; the
browser cannot supply one. Strict schema
`elitea.pipeline.node-recovery-owner-proof.v1` binds execution_id, generation,
journal activation_id, attempt, expected_revision, effect_id, owner_receipt_sha256
and kind `verified_no_effect|committed_result`. Committed result has result_ref:
content_id, immutable_version, digest_sha256, byte_length, media_type,
required_grant_audience. The current HTTP v2 issuer requires content_id=effect_id,
immutable_version=digest_sha256=owner_receipt_sha256, bytes1..1048576,
application/json and audience `elitea.runtime.http-action-receipt.v2`.
A no-effect proof has no result_ref and cannot replace a completed effect result.

The HTTP owner reads the exact immutable frozen request/input and original HTTP
effect under Main's transaction. Journal activation and HTTP effect activation
are distinct; both are checked against their own node/thread/step/definition
contracts. Only a completed validated exact v2 durable receipt produces a result
proof. Missing, failed, dispatching and uncertain records never prove no effect.
Main rechecks byte-identical owner proof at action ACK. A missing owner fails
closed; future Code/no-effect issuers must implement real evidence.

POST `/executions/{executionID}/generations/{generation}/node-recovery/results/{contentID}/versions/{version}`
has empty body/no query and the same current mTLS fence. Main requires the exact
stored authorized action/result_ref; owner rechecks its immutable proof and
returns exact receipt bytes, no effect dispatch or credential/artifact redemption.
Response is application/json, no-store, exact Content-Length and
X-Elitea-Content-SHA256. Main checks bytes/hash/length against stored result_ref.
Worker redeems only that audience and projects through the original admitted
node output contract; artifacts remain descriptors.

## Rollout and evidence

Deploy compatible Main/Worker enums and JSON contracts together. Old Workers
reject unknown desired states/new claim fields. Ordinary HITL, MCP auth, terminal
settlement and new-turn continuation semantics remain unchanged. SQL migrations
have one root-controlled ordering/checksum owner. Component tests do not prove
PG transactions/migrations, real lease replacement, actual graph/journal recovery,
independent services or UI runtime acceptance; those remain root-owned live gates.
