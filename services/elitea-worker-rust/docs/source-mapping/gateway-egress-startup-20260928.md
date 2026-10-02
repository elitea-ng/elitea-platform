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
The retry uses Docker's bundled frontend and completes the existing Go-container build.
The final image is `sha256:021117533df84f88a63466da757579068a838823112704e490fdd18878e8d3b3`, tagged `elitea-llm-gateway:egress-startup-20260928`.
Deployment preserves the three mounts, environment, published port, network aliases, health check, and resource settings.
A synthetic-model probe returns HTTP 200 after the startup correction, instead of the earlier HTTP 502 before dispatch.

Fresh headed-browser chat 702 verifies the temporary cross-compiled image; chat 703 repeats the acceptance on the final container-built image.
Both receive `MODEL_PROVIDER_FAILURE` promptly, retain `VALID_PARTIAL_OUTPUT`, and show the support reference after reload.
No browser responses are mocked and no page errors occur. The chat 702 screenshot is inspected; chat 703 repeats the same browser assertions.
The gateway ledger records a 7 ms stream for chat 702, compared with 120016 ms for chat 699. This is a local observation, not a performance benchmark.
The stream has HTTP status 200 because headers precede the terminal error; the worker failure event is the terminal outcome, not that HTTP status.
The correction closes this incomplete-stream termination case. It does not establish every provider protocol, malformed-stream variant, or Gate 4 requirement.
Evidence prefix: `elitea-incomplete`; the latest result is chat 703. Earlier chat 699 evidence uses prefix `elitea-incomplete-699`.
