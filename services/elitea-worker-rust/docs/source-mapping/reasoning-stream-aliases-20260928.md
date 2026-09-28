# Compatible reasoning stream aliases

## Source mapping

The current SDK selects its provider client in
`elitea_sdk/runtime/clients/client.py::get_llm` and delegates compatible streaming to `ChatOpenAI`.
This is a behavior reference, not a parser implementation to copy.
The Rust owner is `src/transport/openai_compatible_facade.rs::parse_sse_event`.
Regression coverage is in `src/transport/openai_compatible_facade_tests.rs`.

The pinned Bifrost 1.7.15 source provides the exact wire contract.
`schemas/chatcompletions.go::ChatStreamResponseChoiceDelta.MarshalJSON` emits
`reasoning` and `reasoning_content` from the same value.
An empty value can occur in the first stream frame.

## Failure and change

Rehearsal chat 706 starts Full-mode compaction at 956,801 estimated input tokens.
The combined window is 1,000,000 tokens; output and safety reservations are each 8,192 tokens.
The worker rejects the provider stream with `model_gateway.invalid_sse`.
The gateway records HTTP 200 and no completed usage record.

A separate synthetic gateway probe completes with 780,014 provider-reported input tokens and 22 output tokens.
Its first frame contains both reasoning aliases as identical empty strings.
The previous Rust parser rejects every frame containing both aliases.

The parser now accepts identical aliases and consumes their value once.
It still rejects conflicting values.
No request, credential, schema, or permission contract changes.

## Verification and limits

The new regression fails against the original parser with `model_gateway.invalid_sse`.
All 29 compatible-facade tests pass after the change.
Strict library and test Clippy checks pass with warnings denied.
Coverage includes identical empty values, identical nonempty values, and conflicting values.
The test verifies that reasoning is not duplicated and final text remains `OK`.
Deployed compaction acceptance remains pending.

Chat 704 is an invalid synthetic fixture, not product failure evidence.
Its imported responses lack reply links to user messages.
Chat 705 verifies a short Luna request with the same model settings.
Chat 706 corrects those reply links and reaches the provider.
The provider probe proves large-input acceptance, not completed structured compaction.
