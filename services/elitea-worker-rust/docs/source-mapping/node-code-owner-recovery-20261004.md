# Whole-Code owner recovery amendment

This is a private, source-only amendment over the frozen Worker46 node recovery
packet. It does not replace that packet or establish PostgreSQL, transport,
container, Kubernetes, browser or deployment acceptance. No generated bindings
or shared files were edited. The storage source is an unallocated proposal.

The gate remains the explicit per-node resilience requirement in
`docs/remaining-gates.md` gate5b and
`docs/source-mapping/customer-workflow-migration-20260923.md` lines17,44-55,65-77.
Existing platform behavior is the business reference. It does not authorize broad
ADK retries, fabricated outputs or automatic replay of ambiguous external effects.

## Current-to-new mapping

| Existing owning source / behavior | Amendment source / contract |
| --- | --- |
| `sandbox/request.rs` PreparedJob exact transport bytes and domain/length fingerprint | `sandbox/code_recovery.rs` retains raw prepared bytes SHA separately from the existing request fingerprint; compiled intent digest remains distinct. |
| `agents/graph/code_runtime.rs` CodeNode and CodeStateBoundary own source/input/output validation | `agents/graph/code_committed.rs` projects only an authenticated immutable original whole result using the same output contract, without retaining a runtime/client. |
| `agents/graph/code_state.rs` input[] and [messages] select present declared business roots plus built-in input:str | Actual CodeNode/Started-journal/previsit request fixture retains these semantics; messages, framework roots and undeclared values stay outside the prepared input. |
| `sandbox/ledger.rs` reserve/claim/bind/mark_dispatched/finish own lifecycle and lease epochs | `sandbox/ledger_code_recovery.rs` locks the same row for immutable original binding, completed receipt or terminal no-effect seal; it never calls Submit to observe. |
| Existing `expired_hydrations` discovers only reserved inert allocations | `ledger_code_recovery_cleanup.rs` discovers at most 32 original no-effect tombstones per owner, validates the stored seal and binding, and issues a separate terminal-only cleanup lease. `docker_code_recovery_cleanup.rs` terminates the exact retained CID/Pod UID before the shared terminal workspace cleanup hook. |
| `sandbox/ledger.rs` mark_dispatched requires reserved phase/current owner epoch/live lease and rejects cancellation | No-effect seal requires reserved and dispatched_at NULL, expires the owner lease and advances its epoch atomically. An actual SQL race fixture requires exactly one winner. |
| `sandbox/service.rs` and `service_compiled.rs` existing Execute grant, mTLS peer and request validation | Execute-only signed original intent is independently checked and registered before Execute content/dispatch; preparation/Compile/Read/reconciliation cannot carry it. |
| `protocol/sandbox_grant.rs` original role signature and workload identity | `sandbox_code_recovery_grant.rs` adds separate original-intent and owner-observation domains; configured exact Main requester identity is default absent. |
| `protocol/sandbox_compiled_grant.rs` revision4 signed purpose authority | Strict claims42/ref nested scanning and Compile-only original-visit access; nonworkspace absence stays unchanged, other roles refuse it. |
| `agents/graph/compiler.rs` frozen graph policy/digest and node binding | Original YAML/config pins are retained on Code declarations; ordinary default Stop obtains a Started journal writer and preserves legacy dispatch, explicit policy uses attempt dispatch. Scoped Code consumes the actual shared family provider through a typed runtime capability. |
| `agents/graph/code_remote.rs` and preparation/compiled helpers | `code_attempt_remote.rs` pre-admits the original revision1 visit, exports typed debug snapshot, then prepares/selects/finalizes the exact Execute intent before Execute-mode hydration. Raw unadmitted original/scoped invocation is refused. |
| Worker46 node ledger/codec/current writer and Main operator control | `node_recovery.rs` adds a version2 NoEffectReconciled event, preserving the original failure. A separate operator Retry remains required; budgets, denial/cancel and effect identity are unchanged. |
| Existing claim-bound recovery ACK and one-use checkpoint restore | `node_recovery_content.rs` keeps eligible no-effect continuation suspended; terminal settlement and immutable typed handler restore are exclusive branches. Mixed or changed-visit ACK requests are refused before POST. |
| Existing runtime instance/owner lease and broker-owned fixed helper | `ledger_code_platform.rs`/`docker_code_platform_owner.rs` attest only the actual retained original runtime and unchanged owner epoch around bounded fixed read/publish. Original admission binds the immutable launch; read never binds. |

## Original admission and recovery cutpoints

1. Compiler admits the saved declaration and effective recovery policy. The
   actual node writer commits Started before any Code runtime work. The original
   logical visit and effect dispatch are different identities.
2. Main `/code-sandbox/visits` binds that sealed logical visit and exact original
   revision1 prepared bytes to the original saved declaration/current claim. It
   returns a reference and no Execute/recovery authority. Debug uses this
   original stage before workspace/preparation/compilation errors.
3. Owning dependency/workspace/broker extensions select final bytes. Compiled
   selection uses the exact final prepared fingerprint and original selected
   descriptor. `/code-sandbox/intents` freezes that final binding once.
4. Original Supervisor verifies the existing Execute grant plus independently
   signed original intent. It persists immutable binding in the existing
   sandbox_jobs row before Execute hydration or dispatch. A lost response does
   not create no-effect permission.
5. Main owner read/seal uses its configured mTLS client, exact requester/audience
   and short signed current-claim grant. Supervisor checks the original row and
   binding; Main never reads JobLedger/checkpoints directly.
6. Completed owner result requires actual completed/exit0/strict adapter output.
   Failed/cancelled/missing/uncertain/unbound historical rows have no success
   evidence. Worker verifies exact original identity/source/input/visit/attempt,
   receipt bytes and current authenticated action before projection/CAS.
7. Independently proven no-effect is a terminal tombstone before dispatch.
   Original inert runtime cleanup uses its own current epoch/lease and durable
   completion/failure markers. A crash after seal, lease expiry or uncertain
   removal leaves bounded owner-local cleanup pending. The terminal phase and
   dispatch prohibition never change, and the recovery receipt stays byte-identical.
   Dispatch-first refuses seal; seal-first fences every future dispatch. The
   Worker CAS retains original history and business state. Eligible continuation
   stays SUSPENDED until a distinct operator Retry, while typed handler and
   terminal-stop paths require their exclusive authenticated authorization.
8. Retained broker operations require dispatched phase and a live original
   runtime owner. Runtime kind/ID are derived by the owner; each operation checks
   actual owner/epoch/ID before and after. Epoch takeover requires fresh
   attestation, never inferred runtime replacement or restart permission.

Whole-Code OwnerProof effect_id remains the original dispatch. Per-call IDs and
their exact relation remain in the owning call journal and bounded diagnostics.
An unknown call prohibits whole-node replay. A committed call never becomes a
whole-Code result. Verified no-effect excludes every observed call.

## Owners and composition dependencies

- Node recovery owner owns this amendment's Worker authority/codec/projector,
  Supervisor lifecycle routes and versioned owner protocols.
- `http_action_node` owns Main original visit/intent, configured owner mTLS
  client/signer/issuer, current operator ACK/handler topology and compiled access
  request11/claims42 source. Its Code packet is still mutable/unrun.
- `code_workspace_contract` owns common Code declaration/workspace getters,
  repository resolution/hydration, prepared4 and original runtime workspace
  cleanup provenance. Its frozen source patch147a8253956c3e2754239579f257f38f9d8f6f8fbd24009a3b154dc57a164b06
  is composed additively. The activation gate remains false; composition is not
  supported-workspace runtime proof. Future activation still requires preserving
  typed guard/cancel failure through its acquisition and hydration helpers.
- `graph_extensions_closure` owns typed debug exporter; signature is pinned to
  `30748a582fc4844e3a746c368bd578ab552e886ed87efbe83bfb0365efb8f9b2`
  for per-visit admission; BoundCodeTrace is pinned
  `cbb80238b507866ae2ba86d14ae4a5b629191688cfc722a715da51e3aa19fbc0`.
  Each actual attempt binds its immutable original visit; a new retry cannot
  reuse a prior attempt artifact. These modules are external composition inputs.
  Its actual caller-before-preparation regression remains to be executed.
- `data_shaping_nodes` owns prepared5 broker policy, measured image/shim/mailbox,
  Main platform-call journal/step consumers and concrete fixed backend ports.
  Six-path ports freeze `4725a468196bd454eb89205b230ece02be8b31fea95ea1f9df4bee6fabf9cfe9`
  was applied byte-exact privately, then formatted. Readiness stays default false.
- `http_execution_integration` owns the actual registered saved-child scope
  provider, pinned `417b3f779d2d651418b1ae7b57b9899b33f110560f70cef4aaab1a4f0b894eaf`.
  `parallel_node_runtime` owns original opaque Code Arc/provider preservation and
  Code-only registered family materialization. No raw child/root fallback is
  accepted.
- Root owns final additive composition, generated protocol rebuild, migration
  allocation, compiler windows and runtime/deployment verification. Overlapping
  compiler/runtime/ledger files must be composed as surgical hunks.

## Verification status and implementation history

2026-10-04: authored original whole-Code intent/binding codecs, immutable owner
read/seal, reserved-dispatch atomic tombstone, pure original output projector,
durable no-effect journal transition, exclusive ACK branches, original previsit
and compile-only workspace access. Added explicit typed Supervisor guard/cancel
stop classification. Authored actual Code boundary/journal and transport fixture
cases; required PostgreSQL cases are explicitly ignored pending an isolated slot.

- Six private rustfmt passes exited0; pass6 formatted85 authored/private Rust
  files. No compiler/Cargo or dependency rebuild occurred in this amendment.
- Source-only broker ports apply/check and six exact postimage comparisons exited0.
- New tests are authored, uncompiled and unrun. No new pass count is claimed.
- Actual PG dispatch-vs-seal, terminal refusal and immutable completion fixtures
  are authored with an explicit isolated-database requirement; they are not PG
  proof and do not provision a database.
- Frozen Worker46's prior nine offline fixtures remain component evidence only.
  Their pins/results are unchanged and are not reused as this amendment's tests.
- Generated bindings, allocated/applied migration, original Main end-to-end
  issuance, real mTLS owner route, workspace/broker runtime acceptance, measured
  image/helper readiness, actual runtime races, restart and browser/deployment
  acceptance remain separate required checks.


## Separate immutable delivery slices

- cleanup-tombstone-v1 patch98210a2618d5cba7f7cae52adca83ee6fb161fbe608119fccf6b6f2ddc92cbc3,
  freeze7980e9142bd7fc31fa3d3866d25a726d0226f05f009ba880241a36cb4c1567fa.
  Six exact source apply/roundtrips passed; three actual SQL/runtime-double
  fixtures are authored, ignored/unrun. The later workspace terminal hook is a
  separate exact dependency, not evidence that those tests ran.
- original-visit-debug-fixture-v1 patchab406b6032921524f63804491770c7186dc42e385adfb23b3f6fb526b8ad809a,
  freezed17d76e2c262db193e008ef94d44e446c2f3df9c7361fa885e0c605cf7353d72.
  Five exact source apply/roundtrips passed; two actual Remote caller tests are
  authored/unrun. The cfg(test) helper follows real signed-command/Accepted claim
  parsing and one-use take_sandbox_authority, retaining the public conformance
  vector's original historical lease/fence/clock. It does not prove a live lease.
  Actual Started CAS drives /visits, then the real debug serializer/sink contract,
  then a deliberately unavailable Rust preparation profile. Config/dispatch
  changes deny before /visits or any sink upload, with business state preserved.
- Broker observation3 keeps only the original retained process/current owning
  child under the whole-Code deadline after Unknown. It never submits a new
  Code job or child. Deadline uncertainty retains original whole dispatch;
  authenticated denial/cancellation still stop immediately.
- Workspace source composition retains the exact existing shared request
  ancestor510cf6bee663c0faa11a504c02a57f1b8a5a6910ce2f4f1308b142e9bd6869c9,
  adds its strict prepared4/5 producer and preserves matches_code_binding.
  Existing module registrations, broker backend hooks, original compiled
  visibility and recovery cleanup are retained as additive hunks.
- Compiled broker launch2 carries the original Control Execute request digest
  alongside the distinct prepared fingerprint. Plain launch1 bytes stay exact.
  Original registered selector/profile/descriptor validation remains mandatory;
  neither a runtime read nor a later signed observation can select a new binding.
  Matching fixed backend/runner checks are broker-owned external inputs.
