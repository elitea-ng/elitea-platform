# contract/

The app's API surface. The mobile client is generated from the server's OpenAPI document in
this repository, `services/elitea-main/api/openapi/v2.yaml`: there is no pinned copy and no
overlay. When the app needs an operation or a field the spec does not define, add it to the spec
(and the server) in the same change.

| File | What it is |
|------|------------|
| `client-operations.json` | The operationIds kept in the generated client. Everything else in the spec is pruned. |

## Generated client

`scripts/contract.mjs` turns the spec into `src/api/generated/elitea.gen.ts` (committed, types
only, consumed by `openapi-fetch`):

| Command | What it does |
|---------|--------------|
| `npm run contract:build` | prune the spec to `client-operations.json` plus the `$ref` closure, write `contract/.build/pruned.yaml` (ignored) |
| `npm run contract:gen` | build, then run `openapi-typescript` (pinned `7.13.0`) and write the generated file |
| `npm run check:generated-client` | regenerate in memory and fail if the committed file differs (CI, `.github/workflows/ci-mobile.yml`); `--write` regenerates |

The generated file depends only on the PRUNED document (and `info.x-elitea-client-contract`), so
editing a spec operation the app does not use does not make it stale. Server-sent-event
endpoints are typed by hand in `src/api/sse/`.

Until the app moved into this repository (2026-10) it pinned a verbatim copy of the spec, with
an overlay for operations the pin lacked; the history below records which client contract version
brought each operation the app uses.

## Client contract 1.1

Every operation the app uses is defined upstream and tagged `client`. Two were
renamed when they moved from the overlay: `startApplicationTurn` is now
`sendChatMessage` (its body schema `ChatSendRequest`), and `updateNotifications`
is now `markNotificationsSeen`. The path parameter of the transcript and send
routes is `{conversation_id}` (it takes the conversation uuid).

| Area | operationIds |
|---|---|
| Chat | `getConversation`, `createConversation`, `deleteConversation`, `listConversations`, `listConversationMessages`, `sendChatMessage`, `continueChatExecution` |
| Participants | `addConversationParticipants`, `deleteConversationParticipant`, `listParticipantCandidates` |
| Attachments | `uploadConversationAttachment`, `downloadConversationAttachment` (`GET`; the binary body is read as bytes) |
| Tool history | `listMessageTraces`, `getMessageTrace` |
| Agents, skills, toolkits | `listApplications`, `getApplication`, `listApplicationSkills`, `listToolkitInstances`, `listToolkitAvailableTools` |
| Native client | `getClientDiscovery`, `getBrandingPackJSON`, `exchangeNativeToken`, `revokeNativeToken` |
| Who am I (1.2) | `getCurrentAuthor` (`GET /api/v2/social/author`; only the user id is kept) |

Discovery's `attachments` block and the agent progress frames (the
`x-elitea-client-frames` catalogue, typed by hand in
`src/features/chat/frames.ts`) are 1.1 additions; the app gates them on
`client_contract` >= 1.1 (`speaksContract` in `src/api/discovery.ts`) and
treats a 404 or 501 from an older server as "not available".

Contract 1.2 (additive) tags `getCurrentAuthor` `client` as the "who am I"
read, adds the `agent_tool_paused` frame (a tool call that paused for the
user, not a failure) and documents the pause `finish_reason`s on trace steps.
The route and the trace fields exist on a 1.1 server too; a 1.1 server sends
`agent_tool_error` with the interrupt's repr for a pause, which the app still
recognises.

## Client contract 1.3

The server's 1.3 work (elitea-platform PR #1090) is released in v4.7.0. Every 1.3 operation the app uses is tagged `client`:

| Area | operationIds |
|---|---|
| Chat | `cancelChatExecution` (server-side Stop), `regenerateChatMessage`, `getMessageFeedback`, `setMessageFeedback`, `deleteMessageFeedback`, `exportConversation`, `getRecommendations` |
| Notifications | `deleteNotification`, `deleteNotifications` (a deletion reaches the caller's other devices as a `deleted` tombstone in the `changes_since` delta) |
| Memory | `listMemories`, `deleteMemory`, `clearMemories` (the caller's own memories in the project) |
| Usage | `getProjectUsage` (current-period totals; money fields are removed for a member who may not see them) |
| Discover | `listPublicApplications` (the public catalogue: `{rows, total}`, `total` counts the filtered set; rows carry the public `project_id` and the published `version_id`), `getPublicApplication` (welcome message and conversation starters; not tagged `client` upstream, read as optional enrichment: the details screen works without it) |

`NativeClientPolicy` and `ClientPublicPolicy` carry the six data controls
(`allow_share_out`, `allow_share_in`, `allow_cloud_stt`,
`notification_preview`, `allow_notification_actions`,
`allow_system_surfaces`).

The app calls the 1.3 operations only when discovery says `client_contract`
>= 1.3 (`speaksContract(document, 1, 3)`); on an older server the feature is
hidden. The data controls default to the server's own defaults when a server
sends none (src/security/policy.ts `DATA_CONTROL_DEFAULTS`).

Starting a chat with a Discover agent follows the 1.3 server semantics: `createConversation` in the
caller's active project, then `addConversationParticipants` with `entity_meta.project_id` set to
the public project and `entity_settings.version_id` to the published version
(`src/api/queries/discover.ts`).

`GET /api/v2/auth/native/authorize` is a browser navigation, not an API call,
so it is not typed; the app builds that URL itself (`src/auth/nativeAuth.ts`).

## Client contract 1.4

The server's 1.4 work (elitea-platform PR #1139) is released in v4.11.0. The Chats list (elitea-mobile
issue #6) uses:

| Area | What |
|---|---|
| Rename | `updateConversation`: `PUT /elitea_core/conversation/prompt_lib/{project_id}/{conversation_id}` with `{name}` alone (`UpdateConversationRequest`). The server trims the name and answers 400 for a blank one, one longer than 256 characters (code points) or one with a control character (`unicode.IsControl`); the rename sheet sends a name that passes all three (`cleanConversationName`). The answer (`UpdatedConversation`) carries `id` as a string; the app does not read it. |
| Pin | `pinEntity` / `unpinEntity` on `/social/pin/prompt_lib/{project_id}/conversation/{id}` (a shared project pin), tagged `client` from 1.4 |
| Pinned flag | `ConversationSummary.is_pinned` (required from 1.4), also in the `changes_since` delta (a pin re-delivers the row without changing `updated_at`) |

The app calls rename and pin only when discovery says `client_contract` >= 1.4
(`useListActionsAvailable`, `src/features/chats/useChatListData.ts`); on a 1.3 server Pin and
Rename are hidden and Delete and search work as before. A 1.3 server omits `is_pinned`
although the 1.4 type says it is required, so the app reads it as `row.is_pinned === true`.

There is no server-side conversation search in 1.3 or the planned 1.4; the list's search is
the on-device FTS5 index (`src/db/search.ts`).

## Client contract 1.10

| Area | What |
|---|---|
| Archive | `archiveConversation` / `unarchiveConversation`: `PUT` / `DELETE /elitea_core/conversation_archive/prompt_lib/{project_id}/{conversation_id}`, a shared project archive like the pin. Queued in the action queue (`src/sync/conversationOps.ts`, kind `archive`). Offered with `models.chat.conversation.edit` (`permissionList`), as on the web. |
| Archived flag | `ConversationSummary.is_archived` (optional in `src/db/repo.ts`: a row saved from an older server has none). The Chats list and the offline store's reads leave archived rows out. |
| Archived chats | `listConversations` with `archived=only` (and `mine=true`), online only. |

The app's `changes_since` reads do not send `archived`, so the server answers them in the
pre-1.10 shape: an archived conversation is an `access_lost` tombstone and leaves the offline
store, and an unarchived one is a row again.
