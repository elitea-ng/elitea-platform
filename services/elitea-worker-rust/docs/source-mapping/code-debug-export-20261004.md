# Code debug export — 2026-10-04

## Current business behavior

SDK `elitea_sdk/runtime/tools/function.py:227-269` writes a debug snapshot to
`code-debug`; `:519-526` calls it before `tool.invoke`. Denied/unavailable writes
warn and continue. `elitea_sdk/runtime/clients/sandbox_client.py:44-48` can create
that bucket; `_save_code_to_artifact` includes an executable client preamble and
live credentials. These are business references, not replatform authority contracts.
The Rust implementation retains pre-execution inspection and nonfatal export while
using existing bucket ACL and source/selected-input JSON only. It creates no bucket
and copies no executable/client/credential preamble.

Current Rust `graph/code.rs` already parses boolean `debug`, with default false;
`graph/code_runtime.rs` resolves fixed/variable/template source and selects declared
input values. Selected variables are the existing configured approval boundary.
Debug does not discover extra state or add a new state-approval prompt.

## Mapping

| Owning path | Concrete behavior |
| --- | --- |
| `graph/code.rs`, `compiler.rs` | Skipped exact original YAML/final compiler pin; node configuration serialization remains unchanged. Final bind refreshes pin after HTTP/recovery composition changes the compiler digest. |
| `graph/code_debug.rs` | Requires real sealed NodeAttemptAuthority, exact dispatch/config/node/step match and Main OriginalCodeVisitRef; snapshot includes only resolved source and raw selected input. Logical activation differs from per-attempt dispatch. |
| Original visit owner `graph/code_attempt_remote.rs`, `transport/code_intent_content.rs` | Required integration seam: actual Started authority -> original visit -> debug -> workspace/preparation/compile -> separate final dispatch/intent. Ordinary default Stop must use the same real seam. |
| `transport/code_debug.rs`, protocol claim binding | Exact mTLS claim/fence, metadata admit then binary commit, bounded response/ref; no framework authority in bytes. |
| Main `runtime_saved_code_policy.go`, `runtime_saved_code_json.go` | One pure original declaration owner, bounded valid alias resolution, cycle/document/duplicate/null refusal, exact Rust-byte digest, known recovery separation and skip-false broker parity. Only the selected declaration is expanded; merge keys remain literal as in Rust Value parsing. Root lookup excludes owned Map/Parallel nodes; registered child uses sole AllowsNode. |
| Main `runtime_saved_code_input_policy.go` against current `code_state.rs:23-81,119-133`, `compiler.rs:1961-2087`, `yaml.rs:374-380` | Exact saved state-root/type approval, built-in input:str, messages/framework exclusion. Empty/exact [messages] selects present business roots; explicit selection permits present selected subset. No selector-count requirement or checkpoint reads; nested list/dict values remain opaque. |
| Main `runtime_code_debug.go`, `runtime_code_debug_http.go` | Strict source/input snapshot admission and body validation; four requests, fixed caps/timeouts, nonfatal safe response. |
| Main `postgres_code_debug_authority.go` | Checks exact original visit + saved debug/config/request/source/input and current writer identity columns. Reads no Worker checkpoint payload. |
| Main `code_debug_artifacts.go` | Concrete OriginalCodeVisitConsumer reserve/prepare/final callbacks with explicit code_debug purpose, existing bucket ACL/quotas/retention. Each exact original visit/attempt has one reserved artifact; replacement of that visit reconciles it, new retry gets a separate key. No execution/writer row locks across binary/upload; final generation-aware CAS precedes accepted ref. |
| Main `code_debug_cleanup.go` | Bounded maintenance cleanup for unpublished reserved keys; immutable tombstone prevents reallocation/rebinding; object deletion outside SQL locks. |
| `code_trace.rs`, `events_code_debug.rs`, Main `agent_code_debug.go` + narrow trace integration | Data-free partial event, inert digest reference after verified exact visit/attempt receipt, signed execution/generation guard. Same-original-visit replay preserves committed ref; distinct retry and fresh generation traces never inherit it as current. Earlier committed refs remain labeled attempt history. |

## Wire and ownership

Authoritative private wire: `libs/proto/elitea/runtime/v1/code_debug_artifact_v1.md`.
Saved child/family identity belongs solely to Main execution child-scope owner.
Debug consumes that registered purpose through the same original Code visit owner;
it has no nested YAML cache, second visit registry, root fallback or generic write grant.
Actual child runtime binder must forward each exact Main-issued scope; absent scope
is denied. Main does not evaluate graph state or derive the state-bound logical
activation from selected input. Historical retry/continue receipt inspection is an
explicit recovery-owner operation, not new debug publication authority.
The sole Main source producer's complete pre-redemption receipt/fingerprint must
be retained by the original visit for both root and child. It is distinct from the
Rust final compiler definition digest and the Main frozen runtime frame after
static/HTTP freeze. The original visit uses producer-issued `SourceReference` and
the sole owner lookup retains `OriginalSource`; its exact instructions/YAML must
remain unchanged across runtime freezing. A future instruction rewrite refuses
until a separately owned projection contract exists. No full-fingerprint equality
is assumed and cfg/YAML/node alone is not treated as complete source identity.
That concrete producer/DTO assembly remains a root composition prerequisite.

Existing `code-debug` object storage remains mutable under normal authorized bucket
operations. A committed debug reference fixes the expected digest; consumers must
verify fetched byte length/SHA before preview/download. No immutable public storage
version or presigned URL is invented.

## Verification and remaining assembly

All new Rust/Go tests are authored and UNRUN. Executed checks are rustfmt and gofmt
syntax formatting only. No Cargo/Go compile/test/lint, live storage/SQL, deployment
or browser acceptance is claimed. Serializer/event tests prove nothing about an
unassembled production adapter until the owner runs them.

Required combined checks: real ordinary/recovery Started authority and pre-compile
caller ordering; literal/variable/template and four-language exact snapshots;
off/omitted/false unchanged config; actual root/registered child ownership and
purpose; shared anchored fixture parity through the actual owning Rust parser,
cycle/duplicate/multiple-document and selected expansion budgets; current
claim/writer refusal; missing/denied bucket; reservation/upload
uncertainty and response loss; cancellation before final CAS; same-generation
replacement of the same visit, distinct retry publication and attempt history,
fresh-generation refusal; staging cleanup and job-retention order.
Repository adapter tests check the real callback's writer lock stays protected
through short metadata completion and releases before IO, with simulated takeover
before reserve, after external IO and at commit. These authored, unrun port tests
do not replace the owning live PG/fence checks or claim cross-database atomicity.

Root owns final Main route/composition, agentstate pool binding, unslotted migration
number/generated output, maintenance hookup/receipt pruning before job pruning,
registered child Code binder and combined verification. These remain required
assembly work, not a permanent deferral of ordinary/nested Code debug.

## Implementation history

1. Inspected existing SDK snapshot behavior and Rust source/input parser.
2. Authored exact data-only snapshot, durable artifact/ref guard and nonfatal trace.
3. Removed nested YAML cache; used sole Main registered child/family identity.
4. Moved publication to original Code visit before preparation/compile; separated
   logical activation from per-attempt dispatch and included admitted generation.
5. Replaced held-lock upload with short immutable reservation/final-CAS phases and
   bounded unpublished-key cleanup. Pure shared declaration helper gained exact
   recovery-field and optional broker skip-false/null parity.
6. Replaced the initial alias ban with bounded identity traversal and selected-only
   expansion; added one shared saved Code fixture for Main and the actual Rust
   compiler, plus cycle/duplicate/multiple-document and expansion negatives.
7. Found original-visit selector-count mismatch with existing empty/[messages]
   input behavior; added pure exact owning-state input policy and Go/actual Rust
   boundary regressions. Corrected the source-derived shared fixture's reserved
   result root to answer before execution. Prior R4 freeze remains immutable and
   must be superseded; neither freeze claims passed parser execution.
8. Replaced the logical-activation-only publication key with exact immutable
   original visit identity. A newly admitted retry gets its own artifact and
   attempt label; same-visit claim replacement reconciles the earlier artifact.
   Trace merge and durable receipt checks prevent historical refs becoming current.
   Prior consumer-contract1 stays immutable and is superseded by the new derivative.
