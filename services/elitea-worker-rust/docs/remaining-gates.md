# Rust worker remaining gates

Updated: 2026-09-18. Main integration baseline: `1dee0c89`.

This register orders implementation work. The [testing register](testing-gaps.md) owns detailed proof requirements.
Source mappings remain the behavioral evidence for each capability.
Current-platform code defines business behavior, not a requirement to copy its implementation or security defects.

## Progression status

- Gate 1 has pipeline turn, regeneration, and nested-resume fixes with regression coverage. Mixed-guard and collaborative proofs remain open.
- Gate 2 is accepted for progression, with explicit verification debt. OAuth/DCR support is not a claim of complete provider coverage.
- Gate 3 is accepted for progression. The final restricted chat-operation permission check passes on 2026-09-14.
- Gate 4 is active for context management and current-platform runtime drift.
- The [point 3 audit](source-mapping/point3-audit-20260913.md) records the completed acceptance cases and their limits.
- Production Rust capability registration remains disabled.

## Implementation order

| Order | Remaining capability | Closure boundary |
| --- | --- | --- |
| 3a | Internal MCP builder completion | Publish skill and project-context drafting through shared Main services after permission, prompt, edit, and failure-contract checks. |
| 3b | Internal MCP discovery and entity parity | Complete tag relation metadata, Search Options, actor-safe chat operations, and the remaining configuration, skill, application, and secret contracts. |
| 3c | Toolkit discovery and Test | Implement shared `toolkit.available_tools.v1` and `toolkit.call_tool.v1` in Rust. Preserve exact selection, authority, result shape, cancellation, and recovery. |
| 3d | External MCP | Complete dynamic instance schemas and saved-agent/pipeline autonomous terminal, failure, pause refusal, and transport replay proofs. |
| 4 | Current-platform runtime drift | Complete context management, pipeline summaries, dedicated summary models, SDK/UI continuation parity, tool-output editing, provider errors and diagnostics, and same-name toolkit binding. Preserve authoritative skills and project context across compaction. |
| 5 | Remaining graph capabilities | Complete deeper pipeline composition, child variables, static pauses, and isolated Code nodes. |
| 5a | New parallel and map nodes | Implement the separate fixed-branch and data-driven designs with durable child state, bounded concurrency, reducers, and recovery. |
| 6 | Effectful toolkit operations | Require durable intent, effect receipts, idempotency, approval, fencing, and crash reconciliation before writes. |
| 7 | Artifact-backed capabilities | Complete attachment authority, object grants, storage behavior, and affected toolkit operations. |
| 7a | Built-in runtime modules | Complete Attachments, Data Analysis, Image Creation, Ask User, Planner, Python Sandbox, and Smart Tools Selection. Reuse builder contracts and exclude Swarm. Verify runtime behavior and UI controls before indexing. |
| 7b | Long-term user memory | Audit existing memory CRUD/recall and ADK memory/graph-memory integrations. Complete persistent cross-conversation recall, user control, isolation, provenance, and recovery. Keep it separate from execution compaction. |
| 8 | Indexing | Implement indexing after the agent and artifact gates. Indexing remains last. |

Read-only runtime tool binding already works for supported native families.
It does not close standalone editor discovery or toolkit Test.
OpenAPI supports delegated OAuth and client credentials. DCR remains an MCP or other explicitly supported toolkit flow.

## Final point 3 acceptance

The approved temporary role grants only project-view and chat-list access.
Listing succeeds after project admission; all eleven ungranted chat operations return their exact missing permission.
A fresh headed Chrome session creates the PAT; an independent Python client makes the MCP calls.
PAT revocation returns HTTP 204, and the temporary role and membership return to their absent baseline.
See [restricted chat acceptance](source-mapping/internal-mcp-restricted-chat-acceptance.md).
The positive checks cover all eleven ordinary chat operations, including persisted updates and successful participant deletion.
See [chat operation acceptance](source-mapping/internal-mcp-no-content-result.md).

Skill and project-context drafting reuse Main's existing model execution service.
Deployed drafting passes with omitted model settings, selected skill versions, and unchanged saved instructions.
The foreign-version draft failure is deliberate validation evidence.
Restricted-user draft refusal also passes after project admission.
See [draft default-model evidence](source-mapping/internal-mcp-draft-default-model.md).
Keep application drafting unpublished: its legacy operation has `mcp_tool=False`.

Chat-driven entity creation, separate instructions and settings updates, and pinned skill copying have deployed evidence.
The copied agent version loads the exact linked skill version through Rust.
Synthetic credential creation and rotation pass through internal MCP, browser Toolkit Test, and Rust.
See [copied-skill acceptance](source-mapping/internal-mcp-copied-skill-acceptance.md) and [credential acceptance](source-mapping/credential-browser-acceptance-20260914.md).
Chat pipeline creation remains outside the agreed scope.

Toolkit Test passes discovery, authorization-reference exchange, invocation, retry, and pending-authorization reload.
Loss before execution-ID receipt recovers the original result without another provider request.
The active-call crash check proves terminal reconciliation and browser observation under a replacement claim.
It does not prove successful provider continuation or recovery without local state.
Administrative cancellation passes; Toolkit Test needs no new Stop button or cancellation API.
See [cancellation evidence](source-mapping/toolkit-cancellation-acceptance.md).
See [reference binding evidence](source-mapping/toolkit-test-reference-binding.md).
See [discovery authorization](source-mapping/toolkit-discovery-authorization.md), [result recovery](source-mapping/toolkit-test-result-recovery.md), and [request recovery](source-mapping/toolkit-request-recovery.md).

Autonomous external MCP has independent PAT-client evidence for terminal success, failure, pause refusal, and transport replay.
See [fresh external acceptance](source-mapping/external-mcp-fresh-browser-20260913.md).
External elicitation remains [deferred](source-mapping/external-mcp-elicitation-deferred.md).

Preserve the existing application schema and artifact ledger.
Require a concrete ownership or contract need before adding a migration or table.
Continue with one agent. Do not start or resume subagents.

## Active point 4 boundary

Context compaction must preserve a durable continuation checkpoint.
Budget the full model input and reserved output together.
Keep authoritative project context, skill revisions, user instructions, and execution state outside generated summaries.
Use the installed ADK-Rust primitives where they fit these ownership requirements.
The local `context-continuation-design.md` draft records the proposed implementation and replacement proof.
Its verification and delivery remain point 4 work.
Balanced defaults to a 272,000-token total budget, capped by the model's supported context window.
Full uses that supported window. Reserve the admitted maximum output inside either budget.
Track current context use separately from cumulative input/output consumption.
The platform owns the predefined structured summary prompt and validates its JSON contract, including facts, outcomes, unresolved work, next-step hints, and verified references.
User-authored summary text is optional additional guidance, not a replacement prompt or schema.
Long-term user memory belongs to [gate 7b](source-mapping/long-term-memory.md); compaction does not implicitly save memories.
The 2026-09-17 clarification retires legacy numeric context settings from new compaction. Use Balanced or Full without rewriting old records.

Point 4 also includes the following required work, confirmed by the user on 2026-09-14:

- Wire compaction settings, context budget, status, and continuation through UI, Main, and Rust.
- Check the current SDK and UI continuation implementation before changing resume behavior. Cover exhausted output, user Continue, partial output, and replacement without duplicate content.
- Support pipeline summaries and a separately authorized summarization model where the model contract permits it. Preserve its own token and credential limits.
- Inherit context policy for nested agents and applications, with independent occupancy and durable summaries. Recompute capacity against each child's model.
- Apply pipeline compaction to eligible history at model invocations. Preserve exact graph state and keep deterministic nodes free of summarization calls.
- Complete tool-output editing without destroying tool identity, outcomes, or authoritative instruction state.
- Resolve same-name toolkit bindings by exact toolkit and tool identity, using the current SDK fix as behavioral evidence.
- Complete [OBS-RUST-01](source-mapping/agent-runtime.md#obs-rust-01-detailed-runtime-diagnostics): useful public errors, detailed internal causes, synchronous backtraces, async span context, and release-build symbol information. Keep sensitive payloads out of diagnostics.

These are part of gate 4 acceptance, not optional follow-up work after compaction.

The [2026-09-17 toolkit binding audit](source-mapping/toolkit-binding-drift-20260917.md)
checks the newer SDK direct-tool fix. Rust already refuses cross-toolkit fallback;
11 focused checks pass, including two new deterministic direct-node regressions.
This is component evidence, not deployed acceptance of all point 4 contracts.

The [model budget foundation](source-mapping/model-context-budget.md) freezes authorized limits and checks the complete provider request.
It reserves output inside the effective window. Existing frozen inputs remain readable.
Settings delivery, recovery integration, deployed UI verification, and the other point 4 contracts remain open.
The [model compaction implementation](source-mapping/durable-context-compaction.md) adds structured notes and durable preparation checkpoints.
Ordinary child agents now use separate model sessions, inherited policies, and root-fenced PostgreSQL persistence.
Pipeline LLM and model-backed Decision nodes now have independent scoped persistence; mixed and model-free graph component tests pass.
The agent suite passes 350 checks, including PostgreSQL summary reload, writer takeover, complete streamed-result persistence, and scoped compaction progress.
The [dedicated summary-model component](source-mapping/dedicated-summary-model.md) separates authorized model selection, provider binding, and output limits.
[Main settings delivery](source-mapping/context-policy-delivery.md) now resolves presets and summary selections, and preserves admitted continuation policies.
[UI preset controls and the composer indicator](source-mapping/context-policy-ui.md) pass 295 component tests and a fresh headed-browser check with intercepted settings writes.
[Context progress integration](source-mapping/context-progress-events.md) now connects Rust events to Main's response metadata and UI usage/compaction notices.
915 Main checks, eight real-PostgreSQL checks, 386 UI checks, and a fresh browser presentation check pass.
That presentation check used intercepted context status. The later
[deployed acceptance](source-mapping/context-progress-live-20260917.md) verifies
real profile saves, ordinary model-loop measurements, and fresh-browser reload
without intercepted requests. Threshold compaction and recovery acceptance remain
open, as does durable nested activity beyond replay retention.
Main chat shows detailed usage; nested agents show brief compaction activity text, with separate model scopes and no combined parent/child meter.
Nested and graph-model recovery coordination and actual compaction browser acceptance remain open.
The [compatible-provider correction](source-mapping/context-summary-compatibility-20260918.md)
accepts one validated summary object inside presentation text and aligns input fetching with Main's existing 1 MiB contract.
It now requests native structured output and applies bounded evidence-only correction without rewriting summary facts.
The fresh browser run compacts 108,264 estimated tokens to 12,858 and preserves all required facts in worker state.
A second fresh browser session verifies persisted compaction status and stable answer text.
Haiku refuses part of the synthetic continuation despite an intact summary.
A separate GPT Balanced browser run passes all four fact checks and reload after compaction from 242,998 to 54,793 estimated tokens.
The first GPT run with an inherited smaller Haiku summary model fails admission before provider dispatch.
A subsequent split-and-merge implementation passes a fresh browser run with GPT and Haiku, including all four facts and reload.
The retention target is now 15 percent of usable input, with protected-input exceptions.
Fresh browser acceptance reaches 27,886 estimated tokens, or 11 percent, with all four facts and stable reload.
A 400k Full-window browser run now passes after compaction from 353,853 to 40,356 estimated tokens.
All four fact checks and reload pass. Runtime capacity changes preserve application tables.
See [Full context capacity](source-mapping/full-context-capacity-20260918.md).
One-million-token capacity, indivisible oversized records, repeated live compactions, and nested recovery remain open.
The same audit identifies a separate final-output frame limit and an unresolved regeneration measurement difference.
Do not interpret either failure as proof that the model context is full.
The [large-output correction](source-mapping/model-output-delivery-20260918.md) separates completed model answers from individual event limits.
Its focused checks and deployed browser acceptance pass with one 75,629-byte answer, 450 ordered records, and stable reload.
The pipeline container has no separate compaction control.
Model conversations inherit a caller policy when available; otherwise saved children use explicit settings or Balanced defaults.
Plain LLM nodes also compact their accumulated history, as confirmed on 2026-09-16.
See the [fan-out and reduction model](context-continuation-design.md#fan-out-and-model-backed-reduction) for map, parallel, and semantic reduction boundaries.

## Built-in modules before indexing

The user adds gate 7a on 2026-09-14.
Built-in modules are distinct from the internal MCP entity-management tools completed in gate 3.
The [module ledger](source-mapping/builtin-runtime-modules.md) records their source owners, current runtime limits, and required proofs.
Attachments depend on gate 7 artifact authority; Python execution depends on the gate 5 sandbox boundary.
Share these implementations instead of adding a second artifact store or sandbox.
Swarm is excluded from this scope.

The [follow-up sync ledger](source-mapping/main-sync-20260909-followup.md) records the latest merge and migration boundary.

## Delivery checkpoint

Commits `2f206087` and `82e0ef3a` are pushed after the earlier implementation and proof commits.
They reconcile model defaults, API contracts, nested MCP materialization, TLS configuration, and Helm discovery activation.
Source mappings record 112 component checks, 16 form tests, fresh Chrome model-selection checks, and chart verification.
Commit `c6782160` delivers the 24 preserved instruction-authority paths and their required Main wiring after isolated verification.
It passes 82 focused Go checks, eight Python boundary checks, pinned generation, and targeted vet.
Other pending point 4 and point 5 work also remains in the worktree.
The final restricted-operation acceptance now closes point 3 for progression.
See [delivery reconciliation](source-mapping/point3-delivery-reconciliation.md) and [nested materialization](source-mapping/nested-internal-mcp-materialization.md).
Direct pipeline HITL history and editor Test chat belong to gate 5.
Nested HITL controls must not create direct-pipeline history segments.
These gate 5 findings do not block autonomous external MCP acceptance.
The [earlier external acceptance](source-mapping/external-mcp-live-acceptance-20260911.md) remains historical evidence.

## New main changes already incorporated

- Entity-filtered tag listing, relation counts, and internal MCP publication have shared-service and database evidence.
- Version metadata updates merge supplied keys instead of replacing unrelated runtime settings.
- Application timestamps now advance with version changes through tenant migration `0134_application_updated_at.sql`.
- Vault writers use shared locking. Internal MCP retains the shared Main secret handlers.
- Chat participant, canvas, credential, model-grant, and E2E changes are merged.

The [sync ledger](source-mapping/main-sync-20260909.md) records the combined contract and verification boundary.
Merged UI code is not deployed-browser proof.

## Verification debt and production activation

Keep TG-01 through TG-16 open until their completion requirements have evidence.
Priority proofs include sensitive decisions, independent mixed parallel guards, sibling reuse, active-run reauthorization, and competing-tab behavior.
External terminal completion has separate proof from settled authorization pauses.
The selected copied-skill and credential lifecycle cases have deployed evidence; broader builder and editor variants remain separate verification debt.
Preserve complete history across replacement and verify the wider durability requirements independently.
Keep detailed diagnostics under `OBS-RUST-01`.

Production activation requires replacement, reclaim, lost-acknowledgement, Redis TLS/ACL, load, soak, and Kubernetes proofs.
Another worker must continue unfinished work without the original process or its local spool.
Do not count skipped tests, mocks, or a matching final answer as system proof.

## Source ledgers

- [Internal MCP](source-mapping/internal-elitea-mcp.md)
- [External MCP](source-mapping/external-elitea-mcp.md)
- [Toolkit discovery](source-mapping/toolkit-discovery.md) and [toolkit Test](source-mapping/toolkit-test.md)
- [Delegated OAuth/DCR](source-mapping/delegated-oauth-dcr.md) and [active-run expiry](source-mapping/delegated-auth-expiry.md)
- [Agent runtime](source-mapping/agent-runtime.md) and [pipeline nodes](source-mapping/pipeline-nodes.md)
- [Toolkit families](source-mapping/configuration-toolsets.md) and [indexing](source-mapping/indexing.md)

## Gate 5 findings retained from gate 3 verification

Direct pipeline runs require separate review, user decision, and continuation messages.
Approve and Reject preserve their decision labels. Edit preserves the exact submitted text.
HITL review content is static. It does not represent another model generation.
Nested runs show interrupt controls without these separate history segments.
Verify direct participant selection and ephemeral Test chats for agents and pipelines through the deployed UI.
Existing source components do not prove those entry points work.

The Main history-scope classifier is unfinished gate 5 work. Its focused tests pass.
Admission segmentation, UI response rebinding, and deployed history acceptance remain unimplemented or unverified.
Do not deploy provisional-text cleanup as a complete history fix.
Resume this work at gate 5 after gate 4.
