# Pipeline terminal decision output

## Business source mapping

Current SDK revision: `683c8ec5e0433ca9dae05f9e36e95017725d518e`.
`projects/elitea-sdk/elitea_sdk/runtime/tools/hitl.py::HITLNode.invoke` defines approval routing.
Approve and Reject return a route command without generating another answer.
Edit updates the configured state key and routes to another node.
The current SDK rejects Edit routes directly to `END`.

Rust `src/agents/graph/hitl.rs` owns the corresponding native decision node.
`src/agents/graph/resume.rs` validates the stored interrupt and exact checkpoint.
`src/agents/graph/compiler.rs` selects the saved route and final graph result.
`src/agents/events.rs` projects model steps and the terminal browser response.

## Confirmed defect

The [external acceptance record](external-mcp-live-acceptance-20260911.md) records the deployed reproduction.
The original LLM invocation emits the joke before the approval pause.
Approval resumes directly to `END`; no model or data-producing node runs.
The compiler selects the checkpoint's joke as the final result.
A fresh projector has no invocation-local node events and synthesizes another model step.
Main persists both steps, so the duplicate remains after browser reload.

## Implementation

A validated root HITL resume retains an internal root-scope flag.
The compiler checks the exact node and selected route against the saved definition.
Only a direct root route to `END` marks the terminal result as reused.
The marker remains internal to ADK events; the shared wire contract does not change.
The projector retains the final response but emits no new model step for that marker.
Nested decisions and routes to another node retain their existing output path.
No text comparison, global duplicate filter, database migration, or browser suppression is added.

## Verification

An isolated candidate passes 49 pipeline tests and 30 event-projection tests.
Ten compiler tests also pass, including checkpoint-bound approval and another-node routing.
The projection regression verifies both fresh output and reused terminal output.
Both paths retain the complete terminal response.
The reused path emits no new model start, chunk, end, or partial-message step.
Deployment and a repeated browser approval remain required before claiming the live defect fixed.
