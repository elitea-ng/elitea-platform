# API Contract: SPA ↔ Go Backend

This document defines the exact response shapes expected by the EliteaUI SPA for each endpoint served by `elitea-main`. The SPA (RTK Query) is the source of truth — the Go backend must match these shapes exactly.

**Exception: the client contract.** For the operations tagged `client` in `api/openapi/v2.yaml`, the source of truth is the spec plus its committed lock (`api/openapi/client-contract/v1.lock.json`), not the SPA. Native clients ship on their own release cycle, so those operations follow the additive-only rules in [Client contract](#client-contract-adr-0025) below. If the SPA needs one of them to change incompatibly, that change is a contract major bump.

## Response Shape Legend

| Shape | JSON | Used by |
|-------|------|---------|
| **Paginated rows** | `{"rows": [...], "total": N}` | Most paginated lists (apps, skills, toolkits, tags, icons, public apps) |
| **Paginated items** | `{"items": [...], "total": N, "offset": N, "limit": N}` | Configurations only |
| **Plain array** | `[...]` | Authors, trending authors, default icons, permissions |
| **Single object** | `{...}` | Detail endpoints, settings, mutations |
| **Wrapped result** | `{"result": {...}}` | Fork toolkit |

---

## Endpoint Contract Table

### Applications (`/elitea_core/applications/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /applications/prompt_lib/{pid}` | `v2apps.List` | `{"rows": [...], "total": N}` | ✅ OK |
| `GET /application/prompt_lib/{pid}/{aid}` | `v2apps.Get` | Single object `{id, name, version_details, ...}` | ✅ OK |
| `GET /versions/prompt_lib/{pid}/{aid}` | `v2apps.ListVersions` | `{"items": versions}` | ⚠️ NEEDS CHECK — SPA usage unclear |
| `GET /version/prompt_lib/{pid}/{aid}/{vid}` | `v2apps.GetVersion` | Single version object | ✅ OK |

### Public Applications

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /public_applications/prompt_lib` | `coreHandler.PublicApplications` | `{"rows": [...], "total": N}` | ✅ OK |
| `GET /public_application/prompt_lib/{aid}[/{vname}]` | `coreHandler.publicApplicationDetail` | Single object `{id, name, version_details, ...}` | ✅ OK |

### Admin Published Agents

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /admin_published_agents/administration` | `coreHandler.AdminPublishedAgents` | `{"items": [...], "total": N}` | ⚠️ UNKNOWN — admin UI may expect `items` or `rows` |

### Icons

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /upload_icon/prompt_lib/{pid}` | `coreHandler.ListUploadedIcons` | `{"rows": [...], "total": N}` | ✅ FIXED |
| `GET /default_icons/prompt_lib/{pid}` | `coreHandler.DefaultIcons` | **Plain array** `[...]` | ✅ FIXED |
| `GET /upload_skill_icon/prompt_lib/{pid}` | `coreHandler.ListSkillIcons` | `{"rows": [...], "total": N}` | ✅ FIXED |
| `POST /upload_skill_icon/prompt_lib/{pid}[/{vid}]` | `coreHandler.UploadSkillIcon` | icon_meta object `{name, url, size, ...}` | ✅ FIXED |
| `PUT /upload_skill_icon/prompt_lib/{pid}/{vid}` | `coreHandler.UpdateSkillIcon` | `{"updated": true}` | ✅ FIXED |
| `DELETE /upload_skill_icon/prompt_lib/{pid}/{name}` | `coreHandler.DeleteSkillIcon` | `{"ok": true}` | ✅ FIXED |

Skill icons share the agent icons' `icons` bucket and their public
`/icons/{pid}/{filename}` download route; a `skill_` filename prefix separates
the two galleries. See `internal/api/v2/eliteacore/skill_icon.go` for why the
prefix lives in the filename rather than in a key directory.

### Skills (`/elitea_core/skills/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /skills/prompt_lib/{pid}` | `v2skills.List` | `{"rows": [...], "total": N}` | ✅ OK (needs verify) |
| `GET /skill/prompt_lib/{pid}/{sid}[/{vid}]` | `v2skills.Get` | Single skill object | ✅ OK |

### Toolkits (`/elitea_core/tools/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /tools/prompt_lib/{pid}` | `v2toolkits.List` | `{"rows": [...], "total": N}` | ✅ OK |
| `GET /tool/prompt_lib/{pid}/{tid}` | `v2toolkits.Get` | Single toolkit object | ✅ OK |
| `GET /toolkits/prompt_lib/{pid}` | `v2toolkits.ListTypeSchemas` | Type schemas array | ✅ OK |
| `GET /toolkit_types/prompt_lib/{pid}` | `v2toolkits.ListTypes` | `{"rows": [...], "total": N}` | ✅ FIXED |
| `GET /toolkit_available_tools/prompt_lib/{pid}/{tid}` | `v2toolkits.AvailableTools` | `{"tools": [...], "total": N}` | ⚠️ NEEDS CHECK |
| `POST /fork_toolkit/prompt_lib/{pid}` | `v2toolkits.ForkToolkit` | `{"result": {...}}` | ⚠️ NEEDS CHECK |
| `GET /index_meta/prompt_lib/{pid}/{tid}` | `v2toolkits.IndexMeta` | **Plain array** `[...]` | ✅ FIXED |

### Tags (`/elitea_core/tags/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /tags/prompt_lib/{pid}` | `v2tags.List` | `{"rows": [...], "total": N}` | ✅ OK |

### Configurations (`/configurations/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /configurations/{pid}` | `v2configs.List` | `{"items": [...], "total": N, "offset": N, "limit": N, "shared": {...}}` | ✅ OK |
| `GET /configuration/{pid}/{cid}` | `v2configs.Get` | Single config object | ✅ OK |
| `GET /models/{pid}` | `v2configs.ListModels` | `{"items": [...], "total": N}` | ✅ OK |

### Social (`/social/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /social/authors/{pid}` | `v2social.ListAuthors` | **Plain array** `[...]` | ✅ FIXED |
| `GET /social/author/` | `v2social.GetAuthor` | Single object `{id, name, email, ...}` | ✅ OK |
| `PUT /social/author/` | `v2social.UpdateAuthor` | `{"ok": true}` | ✅ OK |

### Trending Authors (`/elitea_core/trending_authors/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /trending_authors/prompt_lib/{pid}` | `coreHandler.TrendingAuthors` | **Plain array** `[...]` | ✅ FIXED |

### Author Detail (`/elitea_core/author/...`)

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /author/prompt_lib/{aid}` | `coreHandler.Author` | Single object `{id, name, email, ...}` | ✅ OK |

### Agent Categories

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /agent_categories/prompt_lib/{pid}` | `coreHandler.AgentCategories` | `{"categories": [...], "total": N}` | ✅ OK (SPA uses `data?.categories`) |

### Recommendations

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /recommendations/prompt_lib/{pid}` | `coreHandler.Recommendations` | `{"applications": [...], "total": N}` | ✅ FIXED |

### Notifications

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /notifications/notifications/prompt_lib/{pid}` | `notificationsapi` (reviewed route; `coreHandler.Notifications` only where it is not composed) | `{"rows": [...], "total": N}` | ✅ OK — `client` contract |

### Users/Roles

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /users/{mode}/{pid}` | `coreHandler.Users` | `{"rows": [...], "total": N}` | ✅ OK |
| `GET /roles/{mode}/{pid}` | `coreHandler.Roles` | **Plain array** `[...]` | ✅ OK |

### Permissions

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /permissions/prompt_lib/{pid}` | `coreHandler.Permissions` | **Plain array** `[{name, enabled}, ...]` | ✅ OK |

### Platform Settings

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /platform_settings/prompt_lib[/{pid}]` | `coreHandler.PlatformSettings` | Single settings object | ✅ OK |

### Project Context

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /project_context/prompt_lib/{pid}/project-context` | `coreHandler.ProjectContext` | `{"content": "...", "enabled": bool}` | ✅ OK |
| `GET /project_info/prompt_lib/{pid}/project-info` | `coreHandler.ProjectInfo` | `{"name": "...", "icon_meta": ...}` | ✅ OK |

### Search Options

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /search_options/prompt_lib/{pid}` | `coreHandler.SearchOptions` | `{"tags": [...], "collections": [...]}` | ✅ OK |

### Chat Config

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /chat_config/prompt_lib/{pid}` | `coreHandler.ChatConfig` | `{"models": [...], "default_model": "..."}` | ✅ OK |

### Collections

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /collections/prompt_lib/{pid}` | `coreHandler.ListCollections` | `{"rows": [...], "total": N}` | ✅ OK |
| `GET /collection/prompt_lib/{pid}/{cid}` | `coreHandler.GetCollection` | Single object `{id, name, applications: {rows, total}, pipelines: {rows, total}}` | ✅ OK |

### Pin/Unpin

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `POST /pin/prompt_lib/{pid}/{type}/{eid}` | `coreHandler.Pin` | `{"ok": true}` | ✅ OK |
| `DELETE /pin/prompt_lib/{pid}/{type}/{eid}` | `coreHandler.Unpin` | `{"ok": true}` | ✅ OK |

### Conversations

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /conversations/prompt_lib/{pid}` | `v2convs.List` | `{"rows": [...], "total": N}` or plain array | ⚠️ NEEDS CHECK |
| `GET /messages/prompt_lib/{pid}/{cid}` | `v2convs.ListMessages` | `{"items": [...], "total": N, "page": N, "page_size": N, "total_pages": N}` | ✅ OK — `client` contract (`listConversationMessages`) |

### Support Assistant

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /support_assistant/config/` | `coreHandler.SupportConfig` | Single config object | ✅ OK |

### Context Manager

| Endpoint | Go Handler | Expected Response | Status |
|----------|-----------|-------------------|--------|
| `GET /context_manager/summaries/{pid}/{cid}` | `v2contextmgr.ListSummaries` | `{"summaries": [...]}` | ⚠️ NEEDS CHECK — unique key name |

---

## Client contract (ADR-0025)

Native clients (mobile and desktop) are built and released separately from a deployment, and one client talks to many deployments of different versions. Decision 6 of ADR-0025 therefore makes the operations they depend on a declared contract.

### What is in it

Every operation tagged `client` in `api/openapi/v2.yaml`. `client` is always the **last** tag, because the web client's generator (orval `tags-split`) files an operation under its first tag. The gate refuses `client` in any other position.

| Area | Operations |
|------|-----------|
| Discovery | `getClientDiscovery` (`GET /.well-known/elitea-client`), `getBrandingPackJSON` |
| Native sign-in | `authorizeNativeClient`, `exchangeNativeToken`, `revokeNativeToken` |
| Device registry | `listNativeDevices`, `revokeNativeDevice` |
| Projects | `listProjects` — `GET /api/v2/projects/project/default/1`. The trailing `1` is a literal path segment, not a filter. The route answers every project the caller can open. |
| Conversations | `listConversations`, `getConversation`, `createConversation`, `deleteConversation` |
| Messages | `listConversationMessages` |
| Chat | `sendChatMessage`, `regenerateChatMessage`, `continueChatExecution`, `uploadConversationAttachment` |
| Streaming | `streamExecutionEvents` (`GET /api/v2/executions/{project_id}/{execution_id}/events`) |
| Notifications | `listNotifications`, `markNotificationsSeen`, `markNotificationSeen`, `streamNotificationEvents` |
| Participants (1.1) | `addConversationParticipants`, `deleteConversationParticipant`, `listParticipantCandidates` |
| Agents, skills, toolkits (1.1) | `listApplications`, `getApplication`, `listApplicationSkills`, `listToolkitInstances`, `listToolkitAvailableTools` |
| Tool history (1.1) | `listMessageTraces`, `getMessageTrace` |
| Attachments (1.1) | `downloadConversationAttachment` |
| Current user (1.2) | `getCurrentAuthor` (`GET /api/v2/social/author`) |
| Stop (1.3) | `cancelChatExecution` (`DELETE /api/v2/elitea_core/task/prompt_lib/{project_id}/{response_message_id}`) |
| Message feedback (1.3) | `getMessageFeedback`, `setMessageFeedback`, `deleteMessageFeedback` |
| Export (1.3) | `exportConversation` |
| Notification deletes (1.3) | `deleteNotification`, `deleteNotifications` |
| Suggestions and discover (1.3) | `getRecommendations`, `listPublicApplications` |
| Memories (1.3) | `listMemories`, `deleteMemory`, `clearMemories` |
| Usage and budget (1.3) | `getProjectUsage`, `getProjectBudget`, `getMemberBudget` |

Contract 1.1 also adds the discovery document's `attachments` policy, typed attachment items on messages (`AttachmentMessageItem`), the `sender` of a `chat_user_mentioned` notification, and the agent progress frame catalogue below. A client gates each on `client_contract` ≥ 1.1 and treats a 404 or 501 from an older server as "not available".

Contract 1.2 adds the client's "who am I" read and a distinct frame for a paused tool call:
- `getCurrentAuthor` (`GET /api/v2/social/author`) answers the caller's `id` (user id), `name` (display name), `email`, `avatar` (a URL, absolute or relative to the deployment origin; `""` when none) and `personal_project_id`. `personal_project_id` is `""` while a fresh account's personal project is still being provisioned (the first read starts it): poll until it is set. Every other key in the answer is outside the client's concern and may be ignored.
- `agent_tool_paused` (below). A tool call that pauses for the user — a sensitive tool awaiting approval, or a clarifying question — is no longer reported as `agent_tool_error`, and its stored trace step (`listMessageTraces`) has `is_error: false` with a pause `finish_reason`. A 1.1 server sends `agent_tool_error` with the LangGraph interrupt as its text for the same call.

Contract 1.3 tags reads and writes the web app already served, describes the server-side stop, and adds data controls to the policy:
- **Stop** (`cancelChatExecution`) stops the run on the server; dropping the stream alone does not, and the run keeps spending the budget. Only the conversation's author or the user who asked may stop it; an invalid project id answers 403 (the project permission check runs first), not 400. A repeated stop by the same caller answers 204 again, also after the run settled; an answer that completed or failed on its own, or is not the caller's, answers 409 without saying which. A stopped turn with no output is removed with its question; one with partial output keeps it. After a 204, close the stream and reload the transcript. Both workers honour it: the run's desired state becomes CANCELLED, which the Python worker reads on its next lease poll or output acknowledgement (a synchronous SDK step already running completes first, and the run still settles as cancelled) and the Rust worker through its lease monitor, which stops the model run (`native_agent_lifecycle.rs` `drive_native_stream`; covered by `output_delivery_tests.rs` `durable_stop_interrupts_only_the_owned_run_then_settles_cancelled` and the Python delivery suites).
- **Regenerate** has one handler, the reviewed agent-execution route. A stub that answered `{"ok": true}` without running anything used to be mounted on the same path and is removed (`TestRegenerateHasOneHandlerTheReviewedOne`).
- **Notification deletes** reach the caller's other devices as `deleted` tombstones in the `changes_since` delta, so a dismissed notification does not come back.
- **Discover**: `listPublicApplications` is the catalogue of published agents (`rows[].project_id` is always the public project; `version_id` is the published version; the icon is in `meta.icon_meta`). To chat with one, create a conversation in the caller's personal project (`createConversation`) and add the agent as a participant (`addConversationParticipants` with `entity_name: application`, `entity_meta: {id, project_id: <row.project_id>}`, `entity_settings: {version_id}`). The turn resolver admits exactly one foreign project, the public one, and only a `published` version (`agent_catalogue_turn_postgres_integration_test.go`); nothing is forked or copied. Any other project's agent is refused at send.
- **Usage and budget** are totals only: billing has no per-model or per-agent dimensions in the budget figures.
- **Data controls** in the policy (public in discovery and full in the token response): `allow_share_out` (copy, share, export; default true), `allow_share_in` (system share sheet into the app; default true), `allow_cloud_stt` (speech recognition that leaves the device; default false), `notification_preview` (`none` or `title`; default `none`; treat an unknown value as `none`), `allow_notification_actions` (default true), `allow_system_surfaces` (titles on widgets and quick actions; default false, which means counts only). The client enforces them; an admin sets them in the `native_client_policy` Configuration section.

The client policy has no operation of its own. Its public part is in the discovery document, and the full policy is `client_policy` in every token response.

`info.x-elitea-client-contract` states the version, for example `"1.0"`. Discovery serves the same value as `client_contract`.

### The additive-only rule

Within one major version, the `client` subset may only grow. `internal/api/clientcontract` enforces this in `go test` (`task test` and the ci-go Test job). It compares the spec with the committed lock of its major, `api/openapi/client-contract/v<major>.lock.json`.

**Breaking changes** (the gate fails):

| Where | Change |
|-------|--------|
| Operation | It is removed, or is no longer tagged `client`. |
| Parameter | It is removed; it changes from optional to required; or a new required parameter is added. |
| Request body | It becomes required; a property is removed; a property becomes required; a new required property is added; an open object becomes closed. |
| Response | A status code, media type, header or property is removed; a required property becomes optional. |
| Any schema | Its `type` or `format` changes; a member is removed from an enum. |
| Nullability | A request field stops accepting `null`; a response field may now be `null`. |
| Request values | An `enum` is added to a request field or parameter that had none; `minLength`, `maxLength`, `minimum`, `maximum`, an exclusive bound, `minItems` or `maxItems` is added or tightened; a `pattern` is added or changed. |

**Additive changes** (allowed): a new operation; a new optional parameter or property; a new response status; a new enum member; a looser request; a constraint on a response field.

Clients must therefore:
- ignore unknown response properties;
- tolerate unknown enum values, unknown SSE event types and unknown `data.type` frames;
- treat an unknown status code by its class (4xx or 5xx).

**When the spec grows**, `TestClientContractLockIsCurrent` fails until the lock is regenerated in the same change:

```bash
cd services/elitea-main
go test ./internal/api/clientcontract -run TestClientContract -update
```

The lock diff is how a reviewer sees the new promise. `-update` refuses to record a breaking change, also when the lock was deleted first (it then compares with the base branch's copy). `TestClientContractLocksKeepTheBaseBranchPromise` compares every lock with the base branch's (`CLIENT_CONTRACT_BASE_REF`, default `origin/main`), so a hand-edited lock cannot carry a breaking change either; CI fetches the base and fails if it is missing.

**A breaking change** is a new major version:
1. Bump `info.x-elitea-client-contract` to `N+1.0`, and bump `discovery.ClientContract` to match. `TestClientContractVersionAgrees` ties them together.
2. Create `vN+1.lock.json` with `-update`. **Keep** `vN.lock.json`; the test refuses a missing older lock.
3. Keep serving the previous version for at least two minor server releases. That is an operational policy; the test can only check that the older lock still exists.

### Chat send and `question_id`

`question_id` is required. The client generates it, and it must be a **lowercase** canonical UUID: an uppercase spelling is refused with 400. It is the turn's admission idempotency key (`internal/application/agentexecution/start.go`, `IdempotencyKey: request.QuestionID`).

- Generate it once per user message and persist it with the unsent message.
- **The same `question_id` with the same body** replays the original admission. The response is 200 with `created: false` and the same `execution_id`, `response_message_id` and `events_url`.
- **The same `question_id` with a different body** answers 409 with `Agent execution request conflicts with an existing turn`.
- A lost response, a timeout or a 503 at capacity: resend the identical request, then follow the `events_url` it returns. Never mint a new `question_id` for a retry.
- A regeneration's key is its own `regeneration_id`, with the same rules.

`events_url` is always a same-origin **absolute path** (`/api/v2/executions/{project_id}/{execution_id}/events`), never a full URL. Resolve it against the deployment origin you signed in to.

`response_message_id` is the `uid` of the assistant message group that the turn writes. It appears in the message list once the turn is admitted.

### Execution event stream

- Each event is `id: <cursor>`, `event: <type>`, `data: <one-line JSON>`. The cursor is a uint64 that strictly increases within the execution.
- **Resume** by sending the last `id` you processed as `Last-Event-ID` or `?cursor=`. Replay is strictly after that cursor, so a resumed stream neither drops nor repeats an event. Sending both with different values answers 400. Sending neither replays from the start.
- The stream is replayed from PostgreSQL, so any replica can serve a resume.
- `execution.replay_reset` means frames before its cursor were pruned. Reload the transcript.
- **The server does not close the stream when a turn ends.** The client recognises the terminal frame and closes the stream itself. Today's frames (`execution.node_event`, whose `data.type` names the frame) are terminal when the frame is one of:
  - `pipeline_finish`;
  - `agent_response` with a non-empty `response_metadata.finish_reason` (an `agent_response` without one is an intermediate answer inside a pipeline);
  - `error`, `llm_error` or `agent_exception`;
  - `mcp_authorization_required` **with** a `response_metadata.authorization_requests` array (without it, the frame is progress);
  - `agent_hitl_interrupt`, **unless** it is a fan-out child pause. A fan-out child pause carries both `response_metadata.metadata.parent_agent_name` and `response_metadata.metadata.child_thread_id`. In that case the child's siblings keep streaming on the same stream, and only the parent's terminal frame ends the turn. An in-process parallel aggregate (`response_metadata.hitl_interrupts` entries that each carry a `parent_agent_name`, but with no `child_thread_id`) **is** terminal;
  - the SSE event `execution.failed` (`code`, `safe_message`, `retryable`), which also covers cancellation and deadline retirement.
- `agent_requires_confirmation` (output limit reached) is not itself terminal. Answer it with `continueChatExecution` and `agent.continue.output-limit.v1`.
- These rules describe the current reference client (`apps/elitea-web/src/features/chat-messages/lib/chatStreamTurnEnd.ts`). A dedicated terminal event is not part of contract 1.x.
- **Robust settle check:** after the stream closes or breaks, read the message list. An answer is settled when its group has no `is_streaming` and `metadata.is_error` is **present**. An absent `is_error` means the turn is still running.

### Agent progress frames (contract 1.1, 1.2)

The `data` of an `execution.node_event` names its frame in `data.type`. The frames a client renders tool activity and pauses from are a catalogue: `x-elitea-client-frames` at the root of `v2.yaml` maps each type to the schema of its `data`, and `internal/api/clientcontract` locks those schemas like a response (additive only). Both workers' unit tests capture these frames into `testdata/client-frames/{python,rust}.json`, and `TestClientFrameCatalogueAcceptsWorkerFrames` validates the captures against the catalogue, so a worker cannot change a catalogued frame silently.

Every frame is `{type, stream_id, message_id, content, response_metadata, created_at, …}`. Ignore keys you do not know.

| `data.type` | `response_metadata` | Terminal? |
|---|---|---|
| `agent_tool_start` | One tool call: `tool_name`, `tool_run_id` (the call's key), `tool_inputs` (the arguments; may be sensitive — show on demand), `tool_meta.name`, `metadata.toolkit_name`/`toolkit_type`/`display_name`/`parent_agent_name`, `timestamp_start`. | No |
| `agent_tool_end` | The same call completed: `finish_reason: stop`, `tool_output` (text), `timestamp_finish`. A client that missed the start builds the call from this frame. When the output was too large for one frame, `tool_output` is `""` and `tool_output_chunks: {total, tool_output_sha256}` names the chunks that came before. | No |
| `agent_tool_error` | The same call failed: `finish_reason: error`, `error`; `content` repeats the error. | No |
| `agent_tool_paused` (1.2) | The same call paused for the user and did **not** fail: `error: null`, `finish_reason` `awaiting_approval` (a sensitive tool), `awaiting_input` (a clarifying question) or `interrupted` (another interrupt), and `pause: {interrupt_id, guardrail_type}` — `interrupt_id` is the one on the `agent_hitl_interrupt` that follows. Show the call as waiting on that card. After the pause is answered, the continued run starts the call again under a new `tool_run_id`. | No |
| `agent_tool_output_chunk` | `tool_output_chunk: {tool_call_id, index, total, tool_output_sha256}`; `content` is the slice. Concatenate the slices of one `tool_call_id` in `index` order; the SHA-256 of the result is `tool_output_sha256`. | No |
| `agent_hitl_interrupt` | `hitl_interrupt` (the pending interrupt: `interrupt_id`, `tool_call_id`, `message`, `available_actions`, `guardrail_type`, `action_label`, `policy_message`, `tool_name`, `toolkit_name`, `tool_args`, `questions`), `hitl_interrupts` (every pending one; a parallel pause has several), `thread_id`. | Yes, except a fan-out child pause (see "Execution event stream") |
| `mcp_authorization_required` | `server_url`, `resource_metadata_url`, `resource_metadata`, `www_authenticate`, `tool_run_id`, `tool_name`, `toolkit_name`, `toolkit_type`. Sent twice: first as progress, then as the terminal frame, which also carries `authorization_requests[]` (one entry per toolkit that needs sign-in) and `thread_id`. The tool has not run. | Only with `authorization_requests` |
| `agent_requires_confirmation` | `finish_reason: length`; `content` is the offered action. The answer stopped at the model's output limit. | No (the answer still settles) |

Group tool calls under `metadata.parent_agent_name` when it is set: a sub-agent ran them.

**Answering a pause** is `continueChatExecution` with the pause's `message_id` (the paused answer) and `thread_id`:

| Pause | `execution_contract` | Body |
|---|---|---|
| One HITL interrupt | `agent.continue.hitl.v1` | `hitl_resume: true`, `hitl_action`, and `hitl_value` per action (below). |
| Several (parallel), or a fan-out child | `agent.continue.hitl.v1` | `hitl_resume: true`, `hitl_decisions: [{interrupt_id, tool_call_id?, action, value?}]`, one per interrupt. |
| Toolkit sign-in | `agent.continue.authorization.v1` | `authorization_request_id` + `authorization_action` (`authorize` or `skip`). `skip` continues without the tool. |
| Output limit | `agent.continue.output-limit.v1` | The paused answer's `message_id`. |

`hitl_value` (and each decision's `value`) by action:
- `approve`, `reject`: absent.
- `edit`: a non-empty **string** — the edited value. For a tool-call pause that is the edited arguments as JSON text.
- `block_with_comment`: a non-empty string, the comment.
- `answer` (an `ask_user` pause): an **object** keyed by question id, each value the chosen option(s) or free text. A plain string is also accepted.

A pause answered elsewhere (another device, the web) answers 409 with an `*_already_resolved` error and `retryable: false`: reload the transcript instead of retrying. After a reload, a pending pause is in the paused answer's `metadata` (`hitl_interrupt`, `hitl_interrupts`, `authorization_requests`, `output_limit_reached`).

### Incremental sync (`changes_since`)

`listConversations`, `listConversationMessages` and `listNotifications` accept `changes_since`:
- **Absent:** the legacy page, unchanged.
- **Empty or `0`:** a full sync in delta shape.
- **Otherwise:** the `next_cursor` from a previous delta of the same list. For conversations, that means the same project and the same `source`, `entity_name`, `entity_meta_id`, `mine` and `hidden`.

The delta response:
- **Rows** are the legacy row shape, oldest change first, plus `tombstones` (`id`, `uuid`, `reason` = `deleted` | `access_lost`, `deleted_at`), `next_cursor` and `has_more`. `total` keeps its legacy meaning: the whole list now.
- **Settle window:** the cursor never passes the database clock minus 5 s, so recently changed rows come back again. **Upsert by id.**
- **Paging:** while `has_more` is true, call again immediately with `next_cursor`. Rows and tombstones are each capped by `limit` (default and maximum 100).

`access_lost` means the item left this list for this caller: it was made private, the caller was removed as a participant, a private conversation an admin listed lost its last user participant, or it no longer matches the filter. A private conversation's deletion is told only to its author, its former user participants and project admins, and to everyone when it was public before it was made private.

Losing access to a whole project is **not** a tombstone. The client gets 403 and drops that project's cache.

Errors:
- **400 `invalid_sync_cursor`:** a cursor from another list or scope.
- **400 `invalid_limit`.**
- **400 `invalid_sync_request`:** notifications only. `changes_since` combined with `offset`, `only_new`, `only_total`, `search`, `event_type`, `sort_by` or `sort_order`.
- **410 `sync_cursor_expired`:** the cursor is older than tombstone retention (`offline_retention_days` cap 90 + 7 = 97 days), or, for conversations, it was issued before the caller gained or lost project admin (which changes what the list shows without any tombstone). Discard that list's cache and resync with `changes_since=0`.
- **501:** a composition without the delta store.

The notification SSE stream (`notifications_ready`, then `notifications_notify`) is a hint. After any reconnect gap, sync the list.

### Native sign-in, devices and policy

- **Verify `iss`** on every authorization response (RFC 9207). It must equal the discovery document's issuer, which defends against mix-up between deployments.
- **Refresh rotates** the token on every use. Serialise refreshes, and persist the new pair durably before you use it.
- **Re-delivery window:** within 30 s (`ELITEA_NATIVE_REFRESH_REDELIVERY_WINDOW`; 0 means strict), presenting the immediately previous refresh token again returns the **same** new pair, so a response lost on a mobile network can be retried safely. Any other reuse revokes the device session.
- **Discriminating a 401:** if `body.error` is the **string** `device_revoked` (the flat ADR body, with `WWW-Authenticate: Bearer error="invalid_token", error_description="device_revoked"`), wipe local data and sign in again. If `body.error` is an **object** (the nested envelope, for example `token_rejected`), refresh once and retry.
- **426 `client_upgrade_required`** (header `X-Min-Client-Version`): send `X-Client-Version: MAJOR.MINOR.PATCH[-pre]` on every request.
  - Only callers holding a native access token, and the native token endpoint, are judged. PAT and cookie callers are exempt.
  - Below the effective minimum (the maximum of the policy's minimum and the client's own) the answer is 426. On the token endpoint the 426 comes before the code or refresh token is consumed.
  - A value that does not parse answers 400.
- **Native endpoints and discovery:** while no native client is registered, the native endpoints answer 404 and discovery says `native_auth: null`.

---

## Confirmed Mismatches (ALL FIXED)

All confirmed mismatches have been resolved.

## Previously Fixed (this session)

| # | Endpoint | Was | Now | Handler Location |
|---|----------|-----|-----|-----------------|
| 1 | `GET /upload_icon/prompt_lib/{pid}` | `{"items":[],"total":0}` | `{"rows":[],"total":0}` | `eliteacore/handler.go:1786` |
| 2 | `GET /default_icons/prompt_lib/{pid}` | `{"items":[...],"total":N}` | `[...]` plain array | `eliteacore/handler.go:1733` |
| 3 | `GET /social/authors/{pid}` | `{"items":[...],"total":N}` | `[...]` plain array | `social/handler.go:166` |
| 4 | `GET /social/trending_authors/prompt_lib/{pid}` | `{"items":[...],"total":N}` | `[...]` plain array | `social/handler.go:197` |
| 5 | `GET /trending_authors/prompt_lib/{pid}` | `{"items":[],"total":0}` | `[...]` plain array | `eliteacore/handler.go:1543` |
| 6 | `GET /recommendations/prompt_lib/{pid}` | `{"items":[...],"total":N}` | `{"applications":[...],"total":N}` | `eliteacore/handler.go:1686` |
| 7 | `GET /toolkit_types/prompt_lib/{pid}` | `{"toolkit_types":[...],"total":N}` | `{"rows":[...],"total":N}` | `toolkits/handler.go:98` |
| 8 | `GET /index_meta/prompt_lib/{pid}/{tid}` | `{"items":[...],"total":N}` | `[...]` plain array | `toolkits/handler.go:422` |

---

## Key Rules for Future Endpoints

1. **Paginated lists with infinite scroll** → `{"rows": [...], "total": N}`
2. **Configurations only** → `{"items": [...], "total": N, "offset": N, "limit": N}`
3. **Non-paginated collections** (authors, trending, default icons, permissions, roles) → **Plain array** `[...]`
4. **Detail/single resource** → Single object `{...}`
5. **Mutations** → `{"ok": true}` or the created/updated object
6. **Context manager summaries** → `{"summaries": [...]}`
7. **Never mix `items` and `rows`** — check the SPA endpoint definition before implementing
