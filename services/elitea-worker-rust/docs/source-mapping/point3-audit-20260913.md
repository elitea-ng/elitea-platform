# Point 3 evidence audit

Status: open. This audit distinguishes implementation, deployed proof, and remaining requirements.
The broad `internal-elitea-mcp.md` closed-category descriptions predate the focused chat and discovery implementations.
Use the focused source ledgers below for current ownership and evidence.
Do not infer closure from an older pending statement or a later successful smoke test.

## Requirements and evidence

| Requirement | Current evidence | Remaining boundary |
| --- | --- | --- |
| 3a skill and project-context drafts | `internal-mcp-draft-default-model.md`: deployed omitted-model drafts, missing-skill refusal, unchanged stored instructions. | Live restricted-actor drafting and selected-version access checks remain unverified. |
| 3b agent creation, separate instructions/settings updates, skill attachment | `chat-internal-mcp-acceptance.md` and `chat-credential-toolkit-creation.md`: persisted changes and saved-agent `load_skill` invocation. | Copied-skill version behavior retains a deployed verification requirement. |
| 3b credential and toolkit lifecycle | `chat-credential-toolkit-creation.md`: chat creation, updates, saved reference, and GitHub invocation. | Synthetic secret lifecycle through browser and worker retains a separate gate. Database-only vault tests do not close it. |
| 3b entity discovery | `internal-mcp-entity-discovery.md`: shared actor-aware Main service, counts and filter tests. Live chat proof appears below. | Nonzero tag counts and restricted visibility have database evidence, not this browser proof. |
| 3b actor-scoped chat operations | `internal-mcp-chat-authority.md`: eleven descriptors and shared handler/database authority tests. | Live operation-level checks remain. Send and continuation are not published; their bounded runtime contract remains implementation work. |
| 3c Toolkit Test | `toolkit-test-reference-binding.md`, `toolkit-cancellation-acceptance.md`, `toolkit-test-result-recovery.md`: invocation, authorization references, administrative cancellation, result/reload observation. | Recovered OAuth authorization after reload retains browser verification. Loss before the client receives an execution ID remains open. |
| 3c active worker crash | `toolkit-test-result-recovery.md`: one provider request, replacement claim 2, durable failure, one browser POST and two GETs. | This proves ambiguous-call reconciliation, not successful continuation or cross-replica spool independence. |
| 3d external MCP | `external-mcp-fresh-browser-20260913.md`: independent PAT calls, saved-agent/pipeline success and failure, pause refusal, mixed guards, exact cursor replay, Main restart cases. | Post-browser-decision replay immutability is not established. Same-project operation permissions need separate live proof. |

Pipeline creation through chat remains excluded by user scope.
Direct pipeline HITL history and participant/Test chat belong to point 5.
No new toolkit Cancel control is required.
Production activation and wider recovery gates remain distinct from point 3 progression.

## Fresh chat entity discovery

A fresh headed Playwright browser opens existing MCP-enabled chat 545.
The user message requests read-only application tags and all five entity option kinds.
Rust calls `get_prompt_lib_tags` and `get_prompt_lib_search_options` through the internal MCP adapter.
Message group 5910 stores successful traces 7424 and 7425 respectively.

The tag result contains zero rows. It does not prove nonzero relation counts.
Search Options returns `application`, `pipeline`, `toolkit`, `credential`, `skill`, `collection`, and `tag` sections.
The rendered tool result contains nine credential rows with only `id` and `name` fields.
The toolkit page contains ten rows and reports a total of 25.
The final answer includes `RUST_GATE3_ENTITY_DISCOVERY_20260913_DONE`.
No entity mutation is requested or performed by these two discovery tools.

Evidence is `elitea-point3-discovery-chat.log`; its script is `elitea-point3-discovery-chat.py`.
The Main service and current-platform mappings are in `internal-mcp-entity-discovery.md`.
Rust retains transport and invocation ownership; Main retains project data and actor visibility.

## Next implementation boundary

Complete the two remaining chat runtime operations through the existing Main execution services.
Preserve actor visibility, exact target identity, bounded observation, and durable execution references.
Do not create an MCP-only execution engine or transfer checkpoint ownership to Main.
Verify the existing eleven chat operations and draft refusal with an actor whose project access differs from operation permission.
A nonmember HTTP 403 cannot substitute for that operation-level proof.
