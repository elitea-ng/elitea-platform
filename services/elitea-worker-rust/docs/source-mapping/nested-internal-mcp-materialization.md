# Nested internal MCP materialization

## Functional reference and source mapping

The SDK reference is `projects/elitea-sdk/elitea_sdk/runtime/toolkits/application.py`, method `ApplicationToolkit.get_toolkit`.
It loads the selected child version and resolves credential placeholders before constructing the child agent.
The replatform preserves the usable child-tool configuration, with Main as the credential owner.
It does not copy the SDK's public-endpoint and secret-expansion path.

| Replatform source | Responsibility |
| --- | --- |
| Main `internal/infra/storage/runtime_application_version.go` | Authorize the parent claim, select the child version, freeze it, and materialize its settings. |
| Main `internal/infra/storage/configurations_materializer.go` | Reuse the existing configured-tool walker and prebuilt MCP resolver. |
| Main `internal/runtimecomposition/composition.go` | Supply the same materializer used for root agent inputs. |
| Rust `src/transport/runtime_context.rs`, `load_application_version` | Read the claim-scoped child version through the runtime API. |
| Rust `src/agents/application_tools.rs` | Assemble the selected child with its resolved tool settings. |

## Implementation

Previously, the nested runtime endpoint returned frozen tool settings without the final runtime MCP URL and authorization header.
Root agent input already used the materializer.
The nested endpoint now uses that same materializer after parent-claim authorization and version freezing.
Project and actor IDs come from the authorized claim.
The request cannot choose those IDs.
The response remains bounded, and materialization errors remain safe runtime errors.
The change adds no database schema or checkpoint owner.

The regression requires at least one internal MCP tool and checks its materialized URL.
The constructor also refuses a missing materializer.
The separate staged project-context assertions remain outside this commit.

## Deployment contracts

The standalone Compose configuration gives Main and workers an HTTPS platform-edge origin for internal MCP calls.
The shared trust bundle contains public roots, the fixture certificate, and the runtime CA.
Its initialization waits for runtime certificate generation.
TLS verification remains enabled.

The Helm capability check exposes a missing toolkit-discovery activation variable.
Main accepts an absent variable as disabled; absence does not itself cause a startup failure.
The chart now exposes `main.runtime.toolkitDiscovery.enabled`, with a default of `false`.
Activation requires the runtime and at least one worker dispatch path.
Render tests verify the default, explicit activation, and both prerequisite failures.

## Verification, 2026-09-14

An isolated candidate excludes the staged instruction-authority and HITL changes.
Twenty-five nested-version and materializer checks pass without skips.
The runtime composition package compiles, and Go vet passes for both owning packages.
These component checks use synthetic credentials and do not establish a complete nested-agent browser flow.

Compose configuration validation passes.
The `task` executable is unavailable, so the exact `helm:lint` task commands run through a local wrapper.
The first run fails because the discovery activation variable is absent.
The corrected capability render suite passes, including four new discovery checks.
The full chart checks also run after the correction.
No Kubernetes deployment occurs during these render checks.

The running rehearsal Main remains healthy with image `sha256:17984c30c0d44c3dd731cfa8694bbd433eacc10a9c0c3561dd69b9304c5acf6d`.
Its internal MCP origin is `https://elitea-platform-edge`.
Its configured trust file is the shared MCP CA bundle.
The browser acceptance and credential reference evidence remain in [point 3 delivery reconciliation](point3-delivery-reconciliation.md).
