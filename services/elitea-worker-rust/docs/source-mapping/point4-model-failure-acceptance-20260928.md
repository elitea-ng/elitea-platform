# Point 4 model-failure acceptance

This matrix separates component contracts from deployed evidence.
A passing transport test does not establish browser or provider acceptance.

## Shared contract

`testdata/proto/runtime/v1/model_failure_policies.json` defines public codes, messages, and retryability.
Rust `protocol/output.rs::model_failure_contract_is_canonical_and_replayable` checks exact replay and rejects injected provider text.
Main `runtimegrpc/output/model_failure_test.go::TestModelFailurePoliciesCrossOutputBoundary` checks persistence admission for the same corpus.
Both tests pass on 2026-09-28.
The corpus includes all ten model/context categories below, plus delivery, pipeline, and input failures.

## Category evidence

| Public category | Component evidence | Deployed evidence and limit |
| --- | --- | --- |
| `MODEL_TIMEOUT` | Compatible header and idle timeout tests; lifecycle timeout classification; HTTP 408/504 cases | Chat 699 preserves partial output and reports the stream idle timeout. |
| `MODEL_RATE_LIMITED` | HTTP 429 classification before body consumption | Chat 697; child failure isolation in chat 728. |
| `MODEL_ACCESS_DENIED` | HTTP 401/403 classification before body consumption | Chat 696; child failure isolation in chat 727. |
| `MODEL_BUDGET_EXHAUSTED` | HTTP 402 classification before body consumption | Chat 729 verifies live failure, support reference, reload, and ERROR-level logging. |
| `MODEL_REQUEST_REJECTED` | HTTP 400 and native unsupported-sampling classification | Chat 730 verifies live failure, support reference, reload, and ERROR-level logging. |
| `MODEL_RESPONSE_INVALID` | Malformed SSE, premature DONE, incomplete native stream, and terminal-order checks | No direct deployed proof for this exact malformed-response category. Chats 702/703 report provider failure after gateway EOF classification. |
| `CONTEXT_BUDGET_EXCEEDED` | Full-request context tests cover each input component before dispatch; lifecycle mapping | Chat 668 verifies a child context failure without terminating its healthy parent. Root admission has component evidence. |
| `MODEL_REQUEST_TOO_LARGE` | `request_profile_and_local_bounds_fail_before_network` verifies serialized request rejection before transport; lifecycle mapping | No direct deployed browser proof for this exact local request-byte category. Input and delivery limits are different categories. |
| `MODEL_UNAVAILABLE` | HTTP 409/503, transport failure, and protocol-version classification | Chat 698 verifies provider HTTP 503. |
| `MODEL_PROVIDER_FAILURE` | Compatible SSE error and native wire-error conversion; provider text exclusion | Chats 731/732 verify compatible/native streamed errors, typed UI guidance, support references, and reload. Chats 702/703 also verify gateway EOF propagation. |

The request-byte and malformed-response categories remain deployed verification gaps.
Do not use chat 695's delivery limit or chat 725's settings limit as substitutes.
The accepted native execution is `1444f90ccc6a281b35899d9e0b399059`; worker logs confirm the native adapter and `anthropic_gateway.provider_error`.

## Source mapping

Current SDK model failures propagate through `elitea_sdk/runtime/tools/llm.py` and nested application tool outcomes.
The current application provides the behavior reference for partial results and parent recovery.
Rust `transport/openai_compatible_facade.rs` and `transport/anthropic_facade.rs` classify provider failures.
Rust `protocol/output.rs` owns the safe public taxonomy.
Rust `execution/native_agent_lifecycle.rs` owns terminal reporting and ERROR-level logs.
Main's runtime output boundary validates canonical public messages before persistence.
Web `ApplicationAnswer` and its error components display partial results and support references.

The native provider-error correction translates Anthropic's `error` wire tag to ADK's `stream_error` enum tag.
It retains the existing ADK type, bounded decoder, and safe error mapping.
It does not introduce retries or change database schemas.

See [continuation diagnostics](continuation-diagnostics-20260923.md) for implementation history and browser evidence.
See [point 4 audit](point4-acceptance-audit-20260928.md) for the remaining closure work.

The five lifecycle taxonomy tests and 24 `ApplicationAnswer` UI component tests also pass on 2026-09-28.
