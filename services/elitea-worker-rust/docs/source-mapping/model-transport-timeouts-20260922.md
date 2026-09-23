# Model transport timeouts

## Functional reference

The current SDK separates model timeouts from ordinary HTTP timeouts.
`projects/elitea-sdk/elitea_sdk/runtime/clients/client.py` sets `model_timeout` to 120 seconds by default.
Its ordinary `timeout` setting defaults to a separate connect/read pair.
The model construction passes `model_timeout` as `request_timeout`.
These references establish separate ownership; the Rust implementation does not copy the Python transport.

## Replatform defect

`src/bootstrap.rs` previously used `content_timeout_millis` for both model response headers and stream inactivity.
The rehearsal content limit is 15 seconds.
Executions `b04fa5111486c3a45afe41b978316246` and `dc032cc935f4e72171d5b523660d6585` encounter that response-header timeout.
The first execution completes compaction before the ordinary chat-model request fails.
This failure does not establish a compaction capacity failure.

## Implementation

`src/config.rs` adds two deployment limits:

| Field | Default | Valid range |
| --- | --- | --- |
| `model_response_header_timeout_millis` | 120000 | 1–300000 |
| `model_stream_idle_timeout_millis` | 120000 | 1–300000 |

Serde supplies the defaults for existing deployment files.
`src/bootstrap.rs` maps these fields to the shared model transport profile.
Both OpenAI-compatible and native Anthropic adapters use that profile.
Content-read and connection deadlines retain their existing settings.
Stream inactivity measures a gap between chunks, not the total generation duration.
Execution cancellation, lease fencing, and execution deadlines remain authoritative.
No database migration or retry of a potentially accepted model request is introduced.

`src/execution/native_agent_lifecycle.rs` maps recognized model transport timeouts to the existing retryable dependency failure category.
It does not report those failures as an execution deadline or expose raw provider text.
More specific user messages remain part of the gate-4 error-contract work.

## Verification

Configuration tests cover backward-compatible defaults, separate settings, valid overrides, zero values, and values above the maximum.
Lifecycle tests cover the timeout codes emitted by both adapters.
Existing transport tests cover response-header and stream-inactivity timeout enforcement.
The nine timeout-filtered tests and the dedicated stream-inactivity test pass.
Strict Clippy, formatting, and diff checks pass.
The first live checks below reveal additional checkpoint and transcript defects.

The release image deploys as `sha256:e8bd3a3a4d46af6a369728abd015894e509a7ca6fd29f2d34b7a68c0d028d726`.
The replacement preserves all five rehearsal worker mounts and existing environment values.
Execution `baf4793a2443f9e341499bf387fd3cf7` completes three live compactions and emits `pipeline_finish` without a terminal failure event.
Estimated input falls from 152,460 to 1,209, then 152,386 to 1,216, then 152,337 to 1,134 tokens.
The answer contains the required facts, but mixes prior attempts with the current read-count requirement.
The browser also detects different answer text after reload.
This run proves repeated live compaction, not full task or transcript acceptance.
Fresh chat 611 removes prior failed attempts from the next verification.

Fresh execution `5a45065900b769fa949118d498f09619` settles as `SUCCEEDED` with exactly 24 tool records and zero tool errors.
It completes two compactions and retains the required delivery code, color, status, next step, and read count.
The final model response treats the checkpoint as potentially separate work and asks an unnecessary clarification.
The streamed transcript includes intermediate narration that disappears after reload.
Both observations identify acceptance gaps, despite correct tool execution and retained facts.

The revised compaction wrapper identifies the replacement as a platform checkpoint, not a new user request.
The summary contract now explicitly preserves task and attempt boundaries.
Fresh chat 612 verifies these corrections with real providers.
Execution `f3420c1127e814465ba192d1617c07c3` completes exactly 24 tool calls and two compactions without tool errors.
The final report preserves the required facts and remains equal after browser reload.
See [terminal answer snapshot](terminal-answer-snapshot-20260922.md) for image identities and UI evidence.
This proof does not close the remaining nested-scope or full-window provider checks.
