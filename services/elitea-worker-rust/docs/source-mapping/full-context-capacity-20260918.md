# Full context capacity

## Evidence

Full-mode chat 596 contains 1,407,395 bytes of fictional history.
Main rejects this request before worker admission with `Invalid agent execution request`.
The previous agent input bound is 1 MiB. Model token capacity does not determine this bound.

## Source mapping

This change implements the new context policy. It does not copy legacy compaction.
Main `internal/application/agentexecution/input_bundle.go` stores the authoritative input.
Main `internal/domain/execution/model.go` defines its byte ceiling.
Rust `agents/protocol.rs` and `protocol/control.rs` enforce the corresponding input contract.
Rust `transport/input_content.rs` fetches this content after claim authorization.
Rust `transport/openai_compatible_facade.rs` bounds the prepared provider request.
Rust `state/postgres_session.rs` stores native ADK session and recovery state.

## Implementation

Agent input and provider request ceilings increase to 8 MiB.
This permits a 4 MiB text fixture plus serialization overhead. Model token checks still apply.
Session state permits 8 MiB. Events permit 16 MiB for combined state and content.
Application and user shared state retain their 1 MiB bounds.
Configuration and direct toolkit input bounds remain unchanged.
Shared migration 0132 and agentstate migration 0003 change runtime constraints only.
No application table changes are required.
Deploy database constraints and Main consumers before the worker producer.

## Outstanding verification

The isolated rehearsal stack now passes the 400k Full-window browser case.
Live process recovery, repeated compaction, and one-million-token capacity remain open.
The 64 MiB retained event limit still needs a separate retention design.
Do not treat larger per-record capacity as unlimited long-run durability.
No live one-million-token model is available in the current test catalogue.

## Initial checks

Main input-bundle tests pass, including an exact 4 MiB history round trip.
PostgreSQL session tests pass with migration 0003 installed in isolated databases.
A 4 MiB checkpoint also survives session-writer replacement. Exact replay stores one event.
Oversized shared app/user updates fail without partial state.
This is a component test, not a live process-crash test.
Protocol generation and whitespace checks pass.

## Rehearsal deployment

Only shared migration 0132 and agentstate migration 0003 were pending. Both applied successfully.
Main image: `sha256:3941189c7bc5f3a74749f8e54263b9aec073ced081774a30f3df95956b38dcdc`.
Worker image: `sha256:a139a8c36538d3b9b5330fa9b654027ae39a25de47f2447c195a42b1ff787529`.
A fresh headed browser submits chat 596 successfully after deployment.
Execution `f5e331881b558a736156d766dcc6bf75` starts compaction at 353,853 estimated input tokens.
Its Full window is 400,000 tokens. Usable input is 387,808 tokens.
Compaction completes at 40,356 estimated input tokens, approximately 10% of usable input.
The delivery code, corrected color, completed check, and next step survive.
The answer remains unchanged after reload. No browser page errors occur.
The browser uses real provider calls without mocked responses.
This proves one Full-mode compaction, not repeated compaction or process recovery.
