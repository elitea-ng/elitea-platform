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

The lock diff is how a reviewer sees the new promise. `-update` refuses to record a breaking change.

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
