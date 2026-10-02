# Runtime limit scope

Status: partial. This inventory separates transport bounds from agent execution policy.
The fragment-count failure is fixed. Large-output and long-session behavior still need proof.

## Behavioral reference

The SDK reference revision is `c0bca04c5a3ef53608932eeb925e4a473175e5f2`.
`runtime/langchain/assistant.py` maps application `meta.step_limit` into the generated LLM node.
`runtime/clients/client.py::create_agent` describes this setting as a tool-execution iteration limit, with default 25.
`runtime/langchain/langraph_agent.py` reports graph step exhaustion separately from model output exhaustion.
`runtime/tools/llm.py` handles bounded nested output continuation separately.

These sources define product behavior, not Rust implementation details.
Provider token limits, tool-loop iterations, nested execution, and transport size must remain separate concepts.
A tool-call batch can contain several calls. One batch is not necessarily one tool operation.

## Current Rust bounds

| Scope | Bound | Source | Assessment |
| --- | --- | --- | --- |
| One projected model event | 256 logical content blocks | `agents/events.rs::bounded_logical_parts` | Adjacent text or thinking fragments count as one block. This is not a cumulative turn counter. |
| One projected event scan | 61,440 raw fragments | `agents/events.rs::bounded_logical_parts` | Bounds empty-fragment scanning without changing stored events. |
| One completion or projected text value | 60 KiB | `transport/openai_compatible_facade.rs`, `agents/events.rs` | Independent of max-output tokens. Larger valid output requires bounded segmentation, not an unbounded allocation. |
| One projected tool value | 40 KiB | `agents/events.rs`, `agents/direct_hitl.rs` | Separate from provider response limits and stored session event limits. Large-result handling remains a gate. |
| One OpenAI-compatible response stream | 4,096 SSE events; 8 MiB raw stream | `transport/openai_compatible_facade.rs` | Per request. Provider fragmentation can affect this separate transport bound. |
| One model tool-call batch | 16 calls | `agents/events.rs`, `transport/openai_compatible_facade.rs` | Not the total calls across an agent lifecycle. |
| Concurrent saved child agents | 8 calls | `agents/application_tools.rs`, `agents/session.rs` | Concurrency bound, not a total execution allowance. |
| Admitted step setting | Default 25; accepted range 1–1,024 | `agents/assembly.rs::validate_step_limit` | Passed to ADK iterations and facade model-turn accounting. Cross-resume and nested accounting need a separate contract review. |
| Saved application assembly | 25 application hops; 3 agent tiers | `agents/application_tools.rs` | Static assembly bounds. These do not prove the requested broader nesting contract. |
| Frozen chat history admission | 999 messages | `agents/assembly.rs::current_text_history` | Input bound, separate from model context tokens. |
| One stored session | 4,096 events; 64 MiB retained event bytes | `state/postgres_session.rs` | Cumulative within the session scope. In-memory model compaction does not itself retire durable events. |
| One stored session event | 2 MiB | `state/postgres_session.rs` | Storage bound. Passing it does not imply the event fits the smaller output transport. |

The session service can tighten its limits. It cannot raise them above the listed hard ceilings.
The facade counts model requests within its bound instance.
This does not establish a durable, shared tool budget across claims and nested agents.

## Fixed failure and evidence

Conversation 532 failed on a completed child response with 285 text fragments and 3,023 characters.
The provider reported `Stop`, not an output-token stop.
The old projector counted each fragment as a separate content block.

Generated tests cover Unicode fragmentation across the 256/257 boundary and up to 2,048 fragments.
Negative tests retain byte, logical-block, and empty-fragment bounds.
The [authorization ledger](delegated-oauth-dcr.md) records complete child results and terminal direct-agent and pipeline proofs.

## Remaining work

- Add safe limit identifiers, observed quantities, and ceilings to failure diagnostics. Never include prompt, credential, or tool content.
- Prove many tool-loop turns without a cumulative fragment-count failure.
- Prove long responses across the facade and output transport boundaries. Preserve complete content through bounded segmentation or authorized result storage.
- Define durable step accounting across resume, parallel children, and graph nodes. Keep model-output continuation separate.
- Define session retention and active-history recovery before the cumulative session ceiling becomes a user-facing dead end.
- Review application assembly and nesting bounds against the requested nesting contract.
- Keep future `enable_automatic_continuation` and `automatic_continuation_step_limit` controls explicit and project-configurable.

Do not raise all constants together. Preserve fencing, backpressure, cancellation, and bounded memory during each change.
