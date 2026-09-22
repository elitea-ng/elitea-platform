# Output continuation capacity

## Current-platform functional reference

Core revision `6c59068503cab7adecfce6bebc19ed8bfaf1af90` uses `utils/token_limit_continuation.py` and `rpc/chat_all.py` for explicit output continuation.
It reads persisted visible output and requests the missing ending.
It separates output exhaustion from HITL and MCP authorization resumes.
An empty visible answer remains eligible when reasoning exhausts the output allowance.
The original request constraints remain applicable across continuation calls.

UI revision `d067a728a6a0a26c1f07d304fe066bd38bf39d98` handles completion metadata in `src/components/Chat/hooks.js`.
SDK revision `966526e8334354366dd161b606d73fe8e204b850` separately handles nested output in `runtime/tools/llm.py::_continue_nested_output`.
That nested behavior requires its own Rust parity check; increasing admission capacity does not establish that parity.

## Confirmed mismatch

Completed model answers support 4 MiB, but Main and Rust reject continued partial answers above 64 KiB.
New tests reproduce rejection at 65,537 bytes before the correction.
The failure does not indicate that the model context is full.

| Boundary | Implementation |
| --- | --- |
| Persisted visible answer and continuation identity | Main `internal/db/queries/agent_chat.sql`, output-limit resolution query |
| Admission and immutable request | Main `internal/application/agentexecution/continue.go`, `input_bundle.go` |
| Protobuf and scalar decoding | Rust `src/agents/protocol.rs` |
| Supported continuation profile | Rust `src/agents/assembly.rs` |
| Missing-ending model instruction | Rust `src/agents/session.rs::output_continuation_prompt` |
| Saved prefix, overlap handling, and recovery projection | Rust `src/agents/events.rs` |

Main and Rust now accept a partial answer up to the existing 4 MiB completed-answer limit.
Rust decodes continuation text as a bounded scalar instead of applying the generic control-field JSON limit.
This does not widen unrelated control fields or permit arbitrary nested JSON.
The complete encoded input retains its separate 8 MiB limit, including history and other fields.
JSON escaping and other request content still count toward that aggregate limit.
Model context admission and compaction remain separate from byte-capacity admission.
No migration, table, or protobuf field is added.

## Verification boundary

Eight Rust continuation checks and all 17 agent-input wire checks pass.
The Main application package passes. Rust Clippy passes with warnings denied.
They cover large partial admission, byte limits, malformed text, Unicode, wire round trips, and exact recovery-prefix reconstruction.
The wire suite's obsolete 1 MiB assertion now uses the existing 8 MiB input contract.
The correction requires deployment and fresh browser verification of Continue, appended output, and reload.
This record does not claim deployed acceptance or completion of point 4.

## Rehearsal deployment

The corrected Main image is `sha256:9bfea8353ac09b53a6a9cb6bbd670789f826339aae3c7f86cd88cc997615c3c8`.
The corrected Rust image is `sha256:8c11be66261e0e7e0885a13335cd5483a56316693dd28c85c1e76234358f7031`.
Main retains its deployed source baseline with only the continuation-capacity change.
Rust uses commit `9012713f` through the auditable release build.
The replacement preserves environment, mounts, networks, and resource limits.
No execution holds an active claim during either replacement.

Initial synthetic chats 622 and 623 fail admission because the fixture selects temperature zero.
The current Main admission contract requires a positive temperature.
These requests do not execute a model or test continuation.
The corrected fixture uses temperature 0.2 and starts chat 624.
The real Haiku route produces an 81,472-byte partial answer and requests Continue.
Fresh browser reload preserves that partial answer exactly.
Continue returns HTTP 422 before another model execution.
This is failed end-to-end acceptance, despite passing capacity component checks.

Read-only database inspection finds Haiku and 16,000 output tokens on the user participant.
The assistant participant has no saved model settings.
`ResolveCurrentAdhocTurn` reads `target_mapping.entity_settings`, which refers to that assistant participant.
The initial request supplies model settings explicitly; `currentContinuationInput` supplies an empty settings object.
`currentAdhocSnapshot` then rejects the missing model name.
The current context-policy restoration occurs later and restores only context settings and the summary model.
The repair must recover the admitted task-model selection and reauthorize it before continuation assembly.
Do not populate the assistant settings merely to bypass this reproduction.
Reuse chat 624 for the corrected Continue check instead of regenerating its partial answer.

The original execution is `bda319ba17e5e73ef47f6882f5e1f9da`.
Browser evidence uses the local `elitea-continuation-live` prefix.
The failure screenshot is inspected visually.

## Separate nested-output audit

Rust `application_tools.rs::drain_child` selects final child text without inspecting the output-limit finish reason.
`model_scope.rs::successful_terminal` also lacks an explicit output-limit exclusion.
The current SDK's nested continuation behavior requires a dedicated reproduction and durable implementation review.
Do not treat the child compaction and crash-recovery proofs as proof of output-exhaustion continuation.
