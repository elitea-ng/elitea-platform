# contract/

The server API contract this app is built against. It is the **only** coupling
between Agent Zefir and the Elitea project: no Elitea source code is copied into
this repository.

| File | What it is |
|------|------------|
| `elitea-v2.yaml` | Verbatim copy of the server's published OpenAPI document. **Never edit it.** |
| `elitea-v2.yaml.sha256` | `shasum -a 256` of the copy; `npm run contract:verify` fails on any change. |
| `LICENSE.elitea` | The upstream Apache-2.0 license text and NOTICE, required to redistribute the copy. |
| `client-operations.json` | The app's declared API surface: the operationIds kept in the generated client. |
| `overlay/native-gap.yaml` | **Our own** OpenAPI fragment for REST operations the pinned contract does not define yet, plus `x-zefir-extend` additions to upstream operations and schemas. Each entry records where its shape was observed (`x-zefir-source`) and what will replace it (`x-zefir-until`). Empty from v4.2.0 (1.1) to v4.3.2 (1.2); it now carries the unreleased client contract 1.3 gap (see below). |

## Generated client

`scripts/contract.mjs` turns these inputs into `src/api/generated/elitea.gen.ts`
(committed, types only, consumed by `openapi-fetch`):

| Command | What it does |
|---------|--------------|
| `npm run contract:verify` | sha256 check of the upstream copy |
| `npm run contract:build` | verify, merge the overlay, prune to `client-operations.json` plus the `$ref` closure, write `contract/.build/merged.yaml` (git-ignored) |
| `npm run contract:gen` | build, then run `openapi-typescript` (pinned `7.13.0`) and write the generated file |
| `npm run contract:check` | regenerate in memory and fail if the committed file differs (CI) |

The build **fails** when the overlay defines an operation or component that
upstream already defines. When the server publishes the missing operations
(ADR-0025 WP5), bump the pin, delete the overlay entries the build names, and
regenerate. Server-sent-event endpoints are typed by hand in `src/api/sse/`.

## Provenance

| | |
|---|---|
| Repository | `elitea-ng/elitea-platform` |
| Commit | `a9cada5eff87b5b56ff5fae91ca092534d2ad0ae` (`origin/main`, tag `v4.3.2`, 2026-10-05; the file is byte-identical to its introduction in `e6be27894`, #1067) |
| Path | `services/elitea-main/api/openapi/v2.yaml` |
| Client contract | `1.2` (`info.x-elitea-client-contract`) |
| SHA-256 | `a0c2a7009c5350f1d84b67a914add32dcd65ad683256088a981ab33701fa1f08` |
| Size | 25,752 lines |
| License | Apache-2.0 (see `LICENSE.elitea`; LICENSE and NOTICE unchanged since `e7585fee2`) |
| Copied | 2026-10-06 |

## Bumping the pinned contract

1. Copy the new file verbatim:
   `git -C <elitea-platform> show <commit>:services/elitea-main/api/openapi/v2.yaml > contract/elitea-v2.yaml`
2. Update the hash: `(cd contract && shasum -a 256 elitea-v2.yaml > elitea-v2.yaml.sha256)`.
3. Update the provenance table above (commit, date, hash, line count), and
   `LICENSE.elitea` if the upstream LICENSE or NOTICE changed.
4. Run `npm run contract:verify`, then regenerate and review the diff of the
   generated client.

## Client contract 1.1 (no overlay)

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

## Client contract 1.3 (overlay, not released yet)

The server's 1.3 work is elitea-platform PR #1090 (branch
`feat/client-contract-1-3`, commit `507a13b0c1fc2cf2cebe7a42ba2516c101c49221`,
2026-10-06). It is not in a release, so the pin stays at v4.3.2 (1.2) and the
gap goes through the overlay:

| Overlay entry | What it is |
|---|---|
| `DELETE /elitea_core/task/prompt_lib/{project_id}/{response_message_id}` (`cancelChatExecution`) | Server-side stop of a running answer. 204 (also when this caller already stopped it), 409 when the answer is no longer running or not the caller's. |
| `x-zefir-extend` on `NativeClientPolicy` and `ClientPublicPolicy` | The six data controls: `allow_share_out`, `allow_share_in`, `allow_cloud_stt`, `notification_preview`, `allow_notification_actions`, `allow_system_surfaces`. |

The other 1.3 operations the app uses are already defined (untagged) in the
1.2 pin and are listed in `client-operations.json`: `getMessageFeedback`,
`setMessageFeedback`, `deleteMessageFeedback`, `exportConversation`,
`getRecommendations`. `regenerateChatMessage` was already tagged `client`.

The app calls the 1.3 operations only when discovery says `client_contract`
>= 1.3 (`speaksContract(document, 1, 3)`); on an older server the feature is
hidden. The data controls default to the server's own defaults when a server
sends none (src/security/policy.ts `DATA_CONTROL_DEFAULTS`).

When a release carries 1.3: copy its `v2.yaml` verbatim (steps below), delete
the overlay entries the build names, regenerate, and update this section.

`GET /api/v2/auth/native/authorize` is a browser navigation, not an API call,
so it is not typed; the app builds that URL itself (`src/auth/nativeAuth.ts`).

## Non-affiliation

Elitea is a trademark of its respective owner. Agent Zefir is an independent
client and is not affiliated with or endorsed by it.
