# Point 3 delivery reconciliation

## Scope

This entry records verified point 3 changes that remained outside earlier commits.
Instruction authority and direct pipeline HITL history remain separate work.
No product database migration is added.

## Source mapping

| Functional reference | Replatform source | Behavior |
| --- | --- | --- |
| `projects/EliteaUI/src/components/EmbeddingModelSelect.jsx` | Web `features/toolkits/ui/form/ToolBase/ModelSelectField.tsx` | Select the configured embedding default when the form has no value. Preserve existing selections. |
| Core `api/v2/search_options.py` and `utils/tags.py` | Main `internal/application/entitydiscovery/service.go`, `api/openapi/v2.yaml` | Describe actor-scoped search sections, pagination, and entity tag counts. |
| Core `api/v2/mcp_oauth_proxy.py` | Main `internal/api/v2/eliteacore/mcp_delegated_tokens.go`, `api/openapi/v2.yaml` | Describe the saved-resource authorization reference returned for Toolkit Test. |
| Core Toolkit Test and SDK toolkit schema discovery | Main `api/openapi/v2.yaml`, `internal/transport/runtimegrpc/control/toolkit_discovery_verifier_test.go` | Describe saved-toolkit discovery, model settings, and grant references. Reject command and scope confusion. |

The current UI supplies model names and marks automatic selection separately from user edits.
The new selector uses the existing authorized model catalogue and form callback.
It does not copy the current UI's fallback replacement of unavailable saved selections.

Rust ownership remains in `src/toolkits/direct_request.rs`, `src/toolkits/discovery.rs`, and `src/execution/toolkit_output.rs`.
Main resolves product access and supplies claim-bound execution data.
The detailed runtime mappings remain in [discovery](toolkit-discovery.md) and [reference binding](toolkit-test-reference-binding.md).
The entity mapping remains in [entity discovery](internal-mcp-entity-discovery.md).

The OAuth contract now includes `authorization_resource`.
Main already returns this field, and the browser already uses it when storing authorization references.
Both generated clients now describe the same field.
The public response still supports the existing access-token exchange mode.

## Verification, 2026-09-14

Sixteen toolkit form tests pass without skips.
They cover default selection, explicit selection, and unavailable saved model names.
TypeScript checking and focused lint pass.
The clean Orval check compares 503 files successfully.
The repository generator updates the clients after the missing resource field is added.
Go generation reproduces the pending source output before that addition.
Fifteen API conformance checks and seven discovery-verifier checks pass without skips.

An isolated copy contains only HEAD and this proposed commit.
All 87 API conformance and control tests pass there without skips.
Its 16 toolkit form tests and TypeScript check also pass.
The pending instruction-authority and HITL changes are absent from that copy.

A fresh headed Chrome session selects GitHub from the toolkit type chooser.
The new form displays `text-embedding-ada-002` without a manual model selection.
Existing toolkit 30 displays the same model after reload.
The browser check does not save a toolkit or change an entity.
Evidence files are `elitea-point3-model-default-browser.log` and the corresponding create and existing-toolkit screenshots.

The first browser attempt stops at an invalid login-state cookie.
The next attempt authenticates but expects the form before selecting a toolkit type.
The corrected browser sequence passes both assertions.
These failed harness attempts do not count as product acceptance.

## Earlier history corrections

Commit `72f509c7` already contains the terminal projection-marker recovery repair.
The credential/toolkit history now points to that commit and the later request-recovery ledger.
The internal-MCP overview now links completed draft and default-secret implementations.
The toolkit authorization note now links the completed resource-binding repair.
Administrative cancellation remains verified; a new Toolkit Test Cancel control is outside the agreed scope.

At this delivery checkpoint, the restricted chat-operation acceptance still requires temporary-access approval.
The user later approves the precise two-grant test.
The [restricted acceptance](internal-mcp-restricted-chat-acceptance.md) passes and closes point 3 for progression.

The [nested runtime follow-up](nested-internal-mcp-materialization.md) records the separate materialization and deployment changes.
