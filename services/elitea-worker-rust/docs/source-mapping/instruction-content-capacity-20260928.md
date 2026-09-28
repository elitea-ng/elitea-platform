# Instruction content capacity

Status: implementation and focused verification pass. Deployed acceptance remains open.

## Confirmed failure

Chat 720 fails before invoking the model with approximately 100 KiB of saved chat instructions.
Execution `a9c60832dd5bd3c3028d6a4fde0228c5` reports `agent_input.resource_exhausted` and reaches `FAILED`.
The generic JSON validator treats instruction text as a control string with a 64 KiB limit.
Agent assembly independently applies that same limit.
Template rendering has another 256 KiB ceiling, with fallback to the original unrendered template.
These boundaries prevent valid instruction content from reaching model-context admission.

## Source mapping

| Current behavior source | Rust implementation |
| --- | --- |
| SDK `runtime/langchain/assistant.py::_resolve_jinja2_variables` renders saved instructions. | `agents/variables.rs` preserves bounded rendering and existing fallback semantics. |
| SDK `runtime/langchain/assistant.py::_compose_system_prompt` includes instructions in the model prompt. | `agents/assembly.rs` admits complete agent instructions for ordinary, saved, and nested agents. |
| Main `application/agentexecution/adhoc.go` freezes chat instructions in the application snapshot. | `agents/protocol.rs` admits content at `application.instructions`. |
| Main freezes saved version instructions under `version_details`. | The same parser admits content at `application.version_details.instructions`. |
| Worker history already uses bounded data-plane content capacity. | Instruction parsing uses that capacity without extending arbitrary metadata strings. |

## Implementation

The complete encoded execution input remains limited to 8 MiB.
The application snapshot also remains bounded by that input ceiling.
Only the two known instruction-value paths receive the larger decoded-string allowance.
Other string values and all object keys retain the 64 KiB control-string ceiling.
Duplicate decoded keys, invalid JSON, excessive nesting, and oversized inputs remain rejected.
Arrays or nested metadata cannot inherit instruction-value privileges.
No JSON serialization round trip is added.

Agent instruction assembly and template output share the 8 MiB instruction ceiling.
This is a byte bound, not an available model-token allowance.
Model-context admission still counts the complete system instructions and reserves response capacity.
Protected content that cannot fit must fail context admission. It must not be silently removed or summarized as ordinary history.
Pipeline YAML retains its existing 64 KiB assembly bound; this change concerns agent instruction content.
Skill and project-context snapshot limits remain separate.
Main's legacy project-context injection ceiling remains unchanged; it is not a model context-window setting.
No dependencies, database schema, provider defaults, or worker concurrency settings change.

## Verification

The input-contract suite passes 18 tests, including large instruction round trips and unchanged metadata rejection.
New assembly coverage checks saved, ad-hoc, and nested agents, plus variable rendering above 256 KiB.
It also checks oversized instructions and preserves the pipeline YAML bound.
All 494 agent tests pass with PostgreSQL enabled. Strict library and test Clippy checks pass.
The final assembly regression also passes with its pipeline-bound assertion.
Deployment exposes a second 64 KiB bound in shared model binding.
Execution `0eef389f287f4b4923328ad0e8322e31` fails there before provider dispatch.
The shared facade validator now uses the same 8 MiB instruction ceiling.
Both native Anthropic and compatible adapters call this validator.
Regression coverage checks the boundary, null rejection, and complete compatible request serialization.
Deployed acceptance remains open until this second correction passes the same browser case.
Do not count the earlier smaller-prefix chat 721 as acceptance of this correction.
