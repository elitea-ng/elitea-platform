# Toolkit discovery API integration

## Source mapping

| Current platform source | Replatform source | Functional contract |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/toolkit_available_tools.py` | `services/elitea-main/internal/api/v2/toolkits/handler.go` | Discover operations for one saved toolkit instance. |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/test_toolkit_tool.py` | `services/elitea-main/internal/application/toolkitcalltool/service.go` | Run an explicit operation with supplied arguments. |
| `projects/elitea-sdk/elitea_sdk/tools/github/__init__.py` | `services/elitea-main/internal/api/v2/toolkits/type_catalogue.go` | Expose credential, embedding, and branch settings from the SDK catalogue. |
| Current Core toolkit RPC dispatch | `services/elitea-main/cmd/elitea-main/main.go`, `internal/api/router.go` | Compose discovery and Test independently from index ingestion. |

Main source paths in the last row are relative to `services/elitea-main`.
The native Rust command handlers consume the shared toolkit protocol.
See [Main discovery](toolkit-discovery-main.md) and [Test context](toolkit-test-context.md) for the worker boundary.

## Implementation history

The September 11 integration connects standalone toolkit services to the public REST handlers.
Rust agent-stream deployments can expose discovery and Test while index ingestion remains disabled.
The handler binds the project, toolkit, and authenticated actor before dispatch.
Unavailable discovery returns a service error. It does not substitute stored toolkit names for callable operations.

Type discovery lists policy-filtered names or one complete settings schema.
Saved credential fields describe `elitea_title` and `private` references.
The GitHub form uses SDK settings when available. Its embedding and branch fields remain visible.
The generated form fixture accompanies this contract change.

Internal MCP route composition remains a separate pending integration change.
This commit does not close point 3 or claim complete provider coverage.

## Verification boundaries

The isolated toolkit package passes 514 test events with PostgreSQL enabled and no skips.
Tests create temporary databases and remove them after completion.
The combined toolkit and Main command run passes 659 test events before database checks.
Three Main environment-dependent checks remain skipped: operator email, provider registration, and mixed-load database pools.
Go vet passes for the toolkit API and Main command packages.
The deployed OAuth Test evidence is recorded in [reference binding](toolkit-test-reference-binding.md).

The current Core synchronous Test handler attempts to stop a task after its wait expires.
Main currently returns a pending execution when its synchronous wait expires.
Wait expiry does not prove worker cancellation. Explicit cancellation and recovery remain open acceptance requirements.
