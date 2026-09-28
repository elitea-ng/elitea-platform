# Gateway egress startup and Rust provider failures

## Observed boundary

Rust chat 699 retains partial text but waits for its model-stream idle timeout when the synthetic provider ends without a terminal model event.
The deployed gateway binary uses Bifrost 1.7.3. The branch pins 1.7.15, which includes provider stream-truncation tests.
Deploying the branch gateway produces HTTP 502 before provider dispatch in chats 700 and 701.
The gateway request ledger records these failures. The synthetic provider journal contains neither request.
An authenticated synthetic-model probe reports `provider_connection_failed`.

## Source mapping

This is a new-platform gateway integration defect. The current Python SDK is a functional reference for reporting failed model calls, not an implementation source for Bifrost initialization.

| Owner | Source | Responsibility |
| --- | --- | --- |
| Gateway | `cmd/elitea-llm-gateway/main.go` | Creates the Bifrost server before loading and attaching authored egress rules. |
| Gateway | `cmd/elitea-llm-gateway/egress_plane.go` | Attaches rules and watches later changes. |
| Gateway | `internal/account/account.go::GetConfigForProvider` | Supplies the private-network decision to self-hosted provider clients. |
| Bifrost 1.7.15 | `providers/vllm/vllm.go` | Configures the dialer when the provider is created. |
| Rust | `transport/openai_compatible_facade.rs::validate_response_head` | Classifies the resulting gateway HTTP failure. |

The initial rule attachment can change the private-network decision after provider clients already exist.
The watcher previously initialized its baseline from that new decision, so it saw no transition and never rebuilt those clients.
The correction compares the decision before and after rule attachment and synchronizes self-hosted clients before startup returns.
It reuses the existing provider refresh interface. It does not change the allowlist, credential authority, model limits, dependencies, or database schema.

## Verification

The startup regression fails before the correction: zero provider refreshes instead of two.
It passes after the correction, including unchanged-decision behavior.
Focused gateway startup and account egress tests pass. The egress race tests pass.
The normal image build times out while resolving the Dockerfile frontend, before compilation.
The image build retry and deployed browser retest are pending at this checkpoint.
Stream-truncation acceptance remains open until the actual gateway/worker/UI path passes.
