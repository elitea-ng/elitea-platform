# Main toolkit discovery

## Current source and history

The current platform reads one saved toolkit and expands its settings for the authenticated actor.
The SDK returns tool names, descriptions, and argument schemas.

| Current source | Replacement source | Contract |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/toolkit_available_tools.py` | `services/elitea-main/internal/api/v2/toolkits/handler.go` | Authorize the actor and use the saved toolkit ID. |
| `projects/centry/pylon_main/plugins/elitea_core/rpc/application.py:get_toolkit_available_tools` | `services/elitea-main/internal/application/toolkitdiscovery/` | Admit a durable discovery command and wait for its accepted result. |
| `projects/centry/pylon_indexer/plugins/indexer_worker/methods/indexer_toolkit_available_tools.py` | `services/elitea-worker-rust/src/toolkits/` | Discover tools through the selected worker implementation. |
| `projects/elitea-sdk/elitea_sdk/tools/__init__.py:get_toolkit_available_tools` | `services/elitea-worker-rust/src/toolkits/` | Return `tools` and `args_schemas` from the actual settings. |
| Main `internal/application/toolkitcalltool/`, origin `d23c51bf` (#823); later `7244490c`, `0a7de34d` | `internal/application/toolkitdiscovery/`, `internal/infra/db/repos/toolkit_available_tools_*` | Preserve atomic admission and durable signed-envelope selection. |

Paths beginning with `projects/` name the source umbrella workspace.
These sources define compatibility evidence. They do not run in the replacement.

## Ownership and authority

Main reloads the toolkit through the shared actor-aware toolkit reader.
Main resolves saved configuration references in reference mode.
Main freezes operator guardrails through the shared policy adapter.
Credential redemption occurs after claim, on the private input data plane.
Caller requests cannot supply settings or runtime policy.

The bundle contains exactly two entries:

- `toolkit-settings`, with role `toolkit.available_tools.settings`.
- `toolkit-runtime-context`, with role `toolkit.available_tools.runtime_context`.

The command contains the toolkit type and the settings entry reference.
The bundle digest also binds the runtime policy entry.
Settings and policy bytes stay outside Redis.
The existing command outbox stores the selected signed command bytes and digest.
Output validation recovers the authoritative toolkit type from those bytes.
The admission digest binds the saved toolkit ID, type, settings, and policy.
Migration `0129_toolkit_available_tools.sql` only extends capability and output-type constraints.
Discovery adds no table or application-data column.

Main accepts the result only through generation-fenced output processing.
The artifact must match the accepted execution, bundle, and settings digest.
The existing `index_result_artifacts` ledger records verified artifact metadata.
The existing ObjectStore holds content bytes. Transfer grants own pending upload cleanup.
The public reader checks project scope, artifact identity, content digest, and accepted success output.
It does not wait for the later settlement receipt before reading accepted content.

## Public contract and activation

REST and internal MCP use the same toolkit handler.
Both require `models.applications.tool.details` and project access.
MCP advertises discovery only when the handler has a configured discovery service.
Missing runtime discovery returns 503.
Stored application-tool relations never become a discovery result.

`ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED` defaults to false.
Rehearsal activation requires the index runtime and durable ObjectStore.
This flag does not prove worker parity or authorize production activation.
The worker capability gate remains a separate release condition.

## Verification and limits

Application tests cover saved actor scope, reference settings, unavailable policy, immutable input roles, changed-policy digests, and accepted artifact reads.
Producer tests cover signed reference-only commands and cross-capability rejection.
HTTP and MCP tests cover identity, permission refusal, schema results, and missing runtime behavior.
Configuration tests cover default-off activation and required runtime dependencies.
A real PostgreSQL test covers atomic admission, durable toolkit identity, exact replay, policy-change conflicts, and idempotent expiry retirement.

The dispatcher follows the existing synchronous tool-run model.
A crash before publication leaves unclaimed work until its durable deadline.
The runtime retires expired unclaimed discovery jobs in batches of 32.
The same bounded loop sweeps orphaned discovery uploads.
No new background publisher reconstructs discovery commands.
The available-tools service does not establish complete toolkit parity or replacement-worker system proof.
