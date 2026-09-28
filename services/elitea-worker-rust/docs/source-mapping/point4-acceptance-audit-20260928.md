# Point 4 acceptance audit

Status: active. Audit baseline: `8a2d5c25b`.

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

## Remaining closure work

1. Verify exact skill and project-context revisions after source edits during an active compacted execution.
2. Reconcile every public model-failure category with its component and deployed evidence.
3. Record the diagnostic capture boundary explicitly, including original dependency locations and uninstrumented async work.
4. Resolve the real-provider cache-hit check without confusing it with verified cache-counter transport.

Instruction-authority tests already preserve an original skill snapshot after transcript loss and changed resume input.
`instruction_authority_tests.rs::activation_survives_transcript_loss_and_changed_resume_snapshot` owns that component case.
`postgres_instruction_pause_survives_process_replacement` adds database-backed process replacement coverage when PostgreSQL is available.
These tests do not establish the combined live source-edit, compaction, and browser workflow.

The 2026-09-28 combined regression now exercises both source types through the actual compactor and model checkpoint writer.
It preserves original bodies and revisions, removes old bulk history, and admits edited context only on a fresh turn.
See [instruction-authority verification](instruction-authority.md#verification).
The combined deployed workflow remains open.

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
