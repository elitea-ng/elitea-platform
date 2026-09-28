# Point 4 acceptance audit

Status: accepted for progression on 2026-09-28. Final audit baseline: `728ab82f6`.

This table consolidates later evidence that supersedes historical pending notes.
It does not convert component coverage into deployed acceptance.
The linked mappings retain source ownership, implementation history, and individual test limits.

## Verified boundaries

| Contract | Evidence | Limit |
| --- | --- | --- |
| Combined context accounting | [Provider accounting](provider-context-accounting-20260928.md), chats 708, 717, 719, 720, 724, 726 | Real-provider cache-hit performance remains unproven. Synthetic nonzero cache accounting passes through the full stack. |
| Structured compaction and retained work | [Native history](context-native-history-retention-20260921.md), [terminal snapshots](terminal-answer-snapshot-20260922.md) | Semantic correctness requires scenario evidence, not schema validation alone. |
| Independent child compaction and recovery | [Nested acceptance](nested-compaction-live-20260922.md), chats 616 and 619 | These cases cover the tested child, sibling, and grandchild arrangements. |
| Pipeline model-local compaction | [Pipeline acceptance](pipeline-model-compaction-live-20260922.md), chat 621 | Exact graph state remains outside compaction. General graph-frontier coverage belongs to gate 5. |
| Bounded continuation, repair, and crash recovery | [Continuation](output-continuation-capacity-20260922.md), chats 625, 632, 636, 637, 640, 653, 681, 682, 716 | Four additional calls remain a hard maximum. Completed responses stop earlier. |
| Same-name toolkit binding and legacy names | [Toolkit identity](toolkit-name-compatibility-20260924.md), chats 683, 688, 691, 692 | Acceptance covers the documented direct-node and model-loop cases. |
| Public provider failures and incomplete streams | [Diagnostics](continuation-diagnostics-20260923.md), chats 696–698, 702, 703 | Other failure categories still require an explicit acceptance disposition. |
| Child failure isolation | Chats 668, 727, 728; details below | Direct pipeline nodes keep terminal failure behavior. Child errors do not authorize automatic repetition. |
| Release stack information and async locations | [Release diagnostics](release-diagnostics-20260923.md), chats 654 and 665 | Capture is optional and bounded. Instrumented async spans do not reconstruct every suspended dependency frame. |
| Input and output boundary guidance | [Input diagnostics](input-admission-diagnostics-20260928.md), chats 723 and 725; diagnostics chat 695 | Four input-section contracts pass component checks. Agent-settings rejection has deployed section-specific proof. |

## Final requirement review

The review checks the gate 4 requirements in `remaining-gates.md` against source mappings and deployed evidence.
Historical pending notes remain implementation history. This review supplies their current disposition.

| Requirement | Acceptance evidence |
| --- | --- |
| UI/Main/Rust context settings, occupancy, and compaction status | Provider accounting chats 717, 720, 724, and 726; full-window continuation chat 708. |
| Balanced and Full capacity, including a million-token window | Full-context capacity chat 600; runtime-memory chats 711–713 compact approximately 956,800 estimated input tokens. |
| Platform-owned structured summaries and additive user guidance | Summary work-order chat 604; native-history chat 612 performs two compactions and retains required facts. |
| Dedicated summary model and bounded summary work | Pipeline compaction chat 621 uses a separate Luna summary model; Full-window and recovery mappings retain its independent limits. |
| Authoritative skill/project identities and revisions | Instruction-authority project 118, chat 2 preserves both frozen revisions through compaction and source edits. |
| Independent child settings, summaries, and recovery | Nested acceptance chats 616 and 619 cover worker replacement, siblings, and grandchildren. |
| Model-local pipeline compaction with exact graph state | Chat 621 performs two compactions and preserves the exact graph marker and downstream output. |
| SDK/UI continuation, bounded retries, partial output, and recovery | Continuation chats 625, 632, 636, 637, 640, 653, 662, 681, 682, and 716. |
| Exact toolkit identity and legacy name compatibility | Direct-node chats 683/691 and model-loop chats 688/692. |
| Public errors, Main projection, operator logs, and release diagnostics | The model-failure matrix covers all ten categories; chats 654/665 verify bounded stack and async-location diagnostics. |
| Child failure isolation and pipeline failure propagation | Chats 668, 727, and 728 retain parent progress; chats 673–678 cover direct failures and downstream suppression. |
| Optional tool-output clearing | Deferred by explicit user direction; its separate source mapping records the scope. |

The [model-failure matrix](point4-model-failure-acceptance-20260928.md) records exact category boundaries.
Chat 733 closes native malformed-response acceptance. Chat 740 closes serialized request-byte acceptance.
Both use fresh headed browser sessions, typed live events, support references, and reload checks.

No gate 4 implementation or required acceptance item remains open in this review.
This disposition does not close the overall Rust-worker goal or the later gates.
The limitations below remain explicit: provider cache savings, uninstrumented async frames, and production concurrency are not proven.
General graph/HITL work and the preserved pending edits move to gate 5.

## Diagnostic capture disposition

Release diagnostics retain source line information for captured synchronous frames and instrumented worker async spans.
Capture occurs at the runner failure boundary. It does not guarantee the original dependency failure location.
Uninstrumented suspended futures do not appear as reconstructed async stacks.
Chats 654 and 665 verify the documented capture behavior and operator logs.
Capture remains optional and bounded, with user-facing correlation references instead of raw stacks.
See [release diagnostics](release-diagnostics-20260923.md) for the source mapping, bounds, and deployed evidence.
This boundary is explicit. It is not a claim of complete dependency-level asynchronous backtraces.

## Cache acceptance disposition

Stable instruction prefixes and provider cache directives pass transport verification.
Nonzero cached-token accounting passes component tests and full-stack synthetic verification in chat 726.
Repeated real-provider calls report zero cache hits. They do not establish cache savings or performance improvements.
The implementation preserves cache eligibility; the provider controls actual cache creation and reuse.
Real-provider cache-hit performance remains unverified and must not appear as an accepted performance claim.
See [provider accounting](provider-context-accounting-20260928.md) for the exact adapter and gateway evidence.

## Latest error audit

Fresh headed browser cases 729 and 730 verify HTTP 402 and HTTP 400 respectively.
Both preserve public guidance and support references after reload, without browser mocks or page errors.
Worker logs report terminal failures at ERROR and exclude the synthetic provider-body canary.
The native stream regression finds a wire-to-ADK tag mismatch: `error` versus `stream_error`.
The parser correction passes all 60 provider-facade tests and the canonical public-error contract test.
Its deployed native-stream acceptance passes in chat 732. Chat 731 verifies the compatible path.
See [continuation diagnostics](continuation-diagnostics-20260923.md#native-streamed-provider-error-classification-2026-09-28) for implementation details.

Instruction-authority tests already preserve an original skill snapshot after transcript loss and changed resume input.
`instruction_authority_tests.rs::activation_survives_transcript_loss_and_changed_resume_snapshot` owns that component case.
`postgres_instruction_pause_survives_process_replacement` adds database-backed process replacement coverage when PostgreSQL is available.
These tests do not establish the combined live source-edit, compaction, and browser workflow.

The 2026-09-28 combined regression now exercises both source types through the actual compactor and model checkpoint writer.
It preserves original bodies and revisions, removes old bulk history, and admits edited context only on a fresh turn.
See [instruction-authority verification](instruction-authority.md#verification).
The combined deployed workflow now passes; see the acceptance below.
Its isolated-project setup exposes [restored-schema and provisioning prerequisites](instruction-live-project-prerequisite-20260928.md).
The project insert is fixed and deployed. The rehearsal PgVector bootstrap is corrected.
The live test preserves frozen source revisions and adopts edited sources on a new turn.
Its event audit finds a batched instruction activation overwrite.
The Rust correction passes the focused regression, 496 agent tests, and strict Clippy.
Worker `42c1ac823` passes fresh headed browser acceptance in project 118, chat 2.
Execution `d416b4202534d3a4e5a23398599eb4e5` retains both original revisions after edits during compaction.
Persisted events retain both activation flags after the paired tool calls.
The answer and provider-derived context meter survive reload.
See [deployed activation correction](instruction-authority.md#deployed-activation-correction).

Optional tool-output clearing remains deferred by user direction.
HITL history, general graph composition, and new pipeline nodes remain gate 5 work.
Long-term memory and customer workflow additions do not block point 4.
Production capability registration remains disabled.

## Child provider-failure acceptance

Date: 2026-09-28. No product code changes occur in this verification.
Main and Rust use deployed revision `5179b5429`. Web uses `e86222c85`.
The synthetic provider uses `86cc3b7b3`; the parent uses the real configured Haiku provider.

| Case | Chat | Execution | Persisted child report |
| --- | --- | --- | --- |
| Child HTTP 401 | 727 | `17ba9861194983709596d02f718c7042` | `MODEL_ACCESS_DENIED`, retryable false, recoverable true, `ask_administrator` |
| Child HTTP 429 | 728 | `f2789012819ee9485a328f25692c5882` | `MODEL_RATE_LIMITED`, retryable true, recoverable true, `verify_before_retry` |

Fresh headed Playwright sessions submit requests and inspect live responses and browser reloads.
No browser responses are mocked.
Both parents finish and explain the child failure, recovery action, and missing partial output.
Each execution persists exactly one child call and one child failure report.
Neither report contains a successful response field.
Persisted events and displayed answers exclude the synthetic provider-body sentinel.
Browser page errors and root failure events remain absent.

The source mapping follows the existing diagnostics implementation:

| Current-platform reference | Rust owner | Verified behavior |
| --- | --- | --- |
| `elitea_sdk/runtime/tools/application.py` and `elitea_sdk/runtime/tool_outcome.py` | `agents/application_tools.rs::child_failure_report` | Return a structured child failure that the parent can handle. |
| Typed model outcomes | `protocol/output.rs::model_failure` | Preserve public category, retryability, and recovery guidance. |
| Provider HTTP status | `transport/model_gateway.rs` and compatible facade | Classify the failure without exposing its response body. |
| Parent continuation after child result | `agents/ordinary_tests.rs::child_model_failure_returns_typed_report_and_parent_completes` | Existing component tests cover 429, 403, and 503. This deployed proof adds 401 and 429. |

The initial chat 727 attempts use a stale model alias and receive synthetic echo responses.
They do not establish child-failure acceptance.
The corrected fixture uses the exact Haiku catalog identity before the accepted execution.
An initial observer compares the entire answer card, including dynamic presentation text, and reports a reload mismatch.
Live and reloaded screenshots show the same answer. A fresh read-only session verifies normalized answer content across reloads.
No execution is repeated to resolve that observer mismatch.

Local evidence:

- `/private/tmp/elitea-child-http-401-proof.mjs`
- `/private/tmp/elitea-child-http-proof.mjs`
- `/private/tmp/elitea-child-http-proof-passed.json`
- `/private/tmp/elitea-child-http-readback.mjs`
- `/private/tmp/elitea-child-http-readback.json`
- `/private/tmp/elitea-child-http-proof-401-live.png`
- `/private/tmp/elitea-child-http-727-readback.png`
- `/private/tmp/elitea-child-http-proof-429-live.png`
- `/private/tmp/elitea-child-http-728-readback.png`

This closes the tested nested access-denial and rate-limit boundaries.
It does not prove every provider category, provider retry policy, or production concurrency target.
