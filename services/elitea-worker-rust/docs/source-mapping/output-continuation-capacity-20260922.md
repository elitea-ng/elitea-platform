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
