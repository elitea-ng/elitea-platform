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
| Memory | `listMemories`, `createMemory`, `deleteMemory`, `clearMemories` (the caller's own memories in the project) |
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

## Client contract 1.11

| Area | What |
|---|---|
| Fork | `forkConversation`: `POST /elitea_core/conversation_fork/prompt_lib/{project_id}/{conversation_id}` with `{message_id?, name?}`; 201 answers the new list row. "Fork" on a Chats row (no body: the whole history) and "Fork from here" on a settled message (`message_id`: the group's UUID). Never queued: it needs a connection. 409 `fork_point_unsettled` and `fork_not_supported` are told apart (`src/api/queries/forkConversation.ts`). |
| Forked from | `ConversationDetail.forked_from`: the thread shows "Forked from <name>" only when `name` is present (the reader can still see the source). |

## Client contract 1.12

| Area | What |
|---|---|
| Activity | `ConversationSummary.activity {state, since}`, `last_activity_at` and the caller's `unread` on list rows and their `changes_since` delta. The Chats row draws its glyph (running, needs you, finished unread, finished) and a bold unread title (`src/features/chats/activity.ts`); each section header counts the loaded rows that need the user (the sections are drawn on the device, so there is no server group count to start from). The offline store keeps the fields with the row (`row_json`). |
| Read markers | `markConversationRead`: `PUT /elitea_core/conversation_read/prompt_lib/{project_id}/{conversation_id}` with no body (read up to now). The thread sends it when it opens, on a new answer and on return to the foreground (`src/features/chat/useMarkConversationRead.ts`); the list row turns read at once and the stored row after the answer, since a read is never re-delivered by the delta. |

The app does not follow the project event stream, so `conversation_activity` events are not
read: a transition reaches the list with the `changes_since` delta and the list's refetch on
foreground or pull to refresh.

## Client contract 1.28

| Area | What |
|---|---|
| Create an agent | `generateApplicationDraftStream`: `POST /elitea_core/generate_application_draft_stream/prompt_lib/{project_id}` answers an event stream of `draft.token` frames, then one `draft.complete` (the validated draft) or one `draft.error {code, status, message}`; a refusal before it opens (400, 503) is plain JSON `{error, code}`. Not a `ResumableStream`: no event ids, a dropped stream is a failed draft (`src/features/agentBuilder/streamDraft.ts`, through the POST form of `src/api/sse/transport.ts`). Refine sends the last turns and the draft as edited (`conversation_history`, `current_draft`). |
| Save | `createApplication` with the reviewed draft as an agent with a `base` version (`agentCreateBody`, `@elitea/client-core/agent-builder`), then a chat with it opens. Only the Elitea branch: a phone has no folder for an `AGENTS.md`. Offered on New chat ("Create an agent") when the workspace speaks 1.28. |

## Client contract 1.44

| Area | What |
|---|---|
| Agent memory (ADR-0050 1b) | `listAgentMemory` (`GET /elitea_core/agent_memory/prompt_lib/{project_id}/{application_id}`, paged with `limit` 50 and `offset`; the server hides notes about other people from a non-manager), `deleteAgentMemoryEntry` (`DELETE .../{entry_id}`; offered on a note about the caller, or any note when the list says `can_delete`) and `setAgentMemoryOptOut` (`PUT /elitea_core/agent_memory_opt_out/prompt_lib/{project_id}/{application_id}` with `{opted_out}`). A refusal is `{error, code}` and the app shows `error` as written (`src/api/queries/agentMemory.ts`). The screen (`src/app/(app)/agent-memory/[id].tsx`) opens from the participant sheet's "What it remembers" on an agent and from a Discover agent, in the active project. Clear, read-one, edit, approve and the history stay web and desktop only and are not kept in the generated client. |

The app opens the screen only when discovery says `client_contract` >= 1.44 (`AGENT_MEMORY_CONTRACT`); on
an older server the entry points are hidden. A 404 on the list without a `code` (the router's
`{error: "not found"}`) reads as a server without the routes; the memory handler's own 404 carries `code`.
A delete answered 404 `not_found` drops the note: it is already gone. The screen pins the project it was
opened in; a project or workspace switch while it is open shows "moved" and reads nothing more.

## Client contract 1.45

| Area | What |
|---|---|
| IT support line (ADR-0052 W6.2) | `support_contact` in `NativeClientPolicy` (the token response's `client_policy`) and in `ClientPublicPolicy` (discovery): one line, at most 200 characters, empty for none. The app shows it as "IT support" in Settings › About and in About (`src/security/supportContact.ts`); the device's managed `support_contact` (`src/security/managedConfig.ts`) wins. Normalised and linked by `@elitea/client-core/support` (at most one `https:`, `mailto:` or `tel:` link). A server before 1.45 sends none, and the row is absent. |

## Client contract 1.46

| Area | What |
|---|---|
| Avatar packs (ADR-0050 decision 6, issue 1835) | `persona.avatar_pack` on a conversation participant carries the version's pack (an `AgentAvatarPackV1` object, read live by the participants read since #1619). The 1.38 lock types the property as a string, so the generated type says `string`: the app reads it as unknown JSON through `avatarPackOf` (`participantAvatarPack`, `src/features/chat/avatarPackMedia.ts`) and ignores anything that is not a valid pack. `getAgentMedia` (`GET /agent_media/{project_id}/{file}`, root-mounted, no credential, never version-gated) is kept for the media the pack names. |

The thread reads the pack only when discovery says `client_contract` >= 1.46 (`AVATAR_PACK_CONTRACT`); on an
older server it shows `meta.icon_meta` as before. Media is downloaded like Discover's icons (SEC-3): the
workspace origin only, no credential, redirects refused, a raster Content-Type, at most 2 MiB, and PNG,
JPEG or WebP bytes; only those bytes reach `<Image>` as a `data:` URI. This build draws a state's own still
(PNG, JPEG, static WebP) with no default motion. It cannot play an animated WebP (React Native's `Image`
does not animate WebP: Android builds with `expo.webp.animated=false`, iOS animates GIF only) or a video
(no video module; expo-video needs a new native build), so an animated WebP state shows the still and a
video state its poster, each with the default motion. An SVG is not drawn (the still, then the old icon).
Reduced motion shows the still only and downloads nothing else.

## Client contract 1.51

| Area | What |
|---|---|
| Name and description limits (issue 1968) | An entity name (agent, pipeline, toolkit, credential, token, folder, conversation, skill) is at most 128 characters and a description at most 2000, counted in characters. A longer one answers 400 with `field` and `max_length` next to `error` (`ErrorResponse`). The app shows a counter and blocks the save past the limit (`src/features/forms/textLimits.ts`); the limits come from `@elitea/client-core`. A server before 1.51 stored longer text. |

## Client contract 1.50

| Area | What |
|---|---|
| Nested sub-agent lanes (ADR-0050 decision 4, issue 1872) | At `subagents.max_depth` 2 a `started` entry may carry `depth` (1 or 2, absent reads as 1) and, at depth 2, `parent_agent_id`. A stored `SubAgentLane` may carry `children` (`SubAgentLaneChild`, one level, never with children of its own). The app has no tree view: a grandchild's tool steps follow its parent's under a label built from both ("Owl · temporary › Hawk · temporary", `src/features/chat/subAgentLanes.ts`). A server before 1.50 sends none of these. |

## Client contract 1.49

| Area | What |
|---|---|
| Avatar pack typed (#1881) | `persona.avatar_pack_v1` is an `AgentAvatarPackV1` object, so the generated type is the pack. `persona.avatar_pack` stays the string 1.38 reserved and is deprecated; a 1.49 server never sends it, so a client built for 1.46 to 1.48, which reads only `avatar_pack`, loses the pack (and shows the old icon) until it is updated. The app reads `avatar_pack_v1`, then the object a 1.46 to 1.48 server sent as `avatar_pack` (`participantAvatarPack`), and validates either with `avatarPackOf`. |

Issue 1864: the app plays a state's own file. An image or an animated WebP is drawn with expo-image, which
animates WebP on both platforms; an MP4 or WebM loop plays muted and looping with expo-video, over its
poster until its first frame, with no controls, never taking the audio session, and not keeping the screen
on. Both modules need a new native build. Every file is still downloaded the SEC-3 way, now checked against
the type its extension names, and written to `<caches>/agent-media/<digest of origin>/<sha256>.<ext>`
(issue 1882): the view gets the file URI, a file already there is not downloaded again, and the folder is
deleted with its workspace. Reduced motion still downloads and shows only the still, drawn without playing.

## Client contract 1.48

| Area | What |
|---|---|
| Cloud sub-agent lane (ADR-0050 decision 4, #1823) | A cloud run's temporary sub-agents (`spawn_agent`) stream as `agent_subagent` frames on the answer's own `execution.node_event` stream: `response_metadata.subagent_v1` is one entry (`started` with `display_name`, `avatar`, `label`, `parent_call_id`; `tool_start`; `tool_end`; `text`; `finished` with `status`; `resumed`). The answer keeps the folded lanes as `metadata.subagents_v1` (`SubAgentLane`), so a reload shows them. The app folds both with `@elitea/client-core/stream` (`foldSubAgentEntry`, `parseSubAgentLanes`) and shows each child's tool calls, then its final answer, in the tool card under "<name> · temporary" (`src/features/chat/subAgentLanes.ts`); its in-between text is web and desktop only for now. A server before 1.48 sends neither, and nothing shows. |
