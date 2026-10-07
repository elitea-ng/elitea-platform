# Code chat reload authors and participant selection

Date: 2026-10-07. Status: local source and focused Web checks pass.
The rollout owner retains image delivery and deployed browser acceptance.

## Current to new source mapping

| Current source authority | New Web owner | Result |
| --- | --- | --- |
| Main `internal/api/v2/conversations/handler.go::Participant` and message projection retain numeric participant and author IDs. | `apps/elitea-web/src/entities/message/lib/wire.ts::MessageParticipantWire` | Retain optional persisted participant names without changing the public API. |
| `entities/message/lib/normalise.ts::isParticipant` already compares both ID spellings through `String`. | `features/chat-messages/lib/convertMessagesToChatHistory.ts` | Resolve each assistant row's persisted author through the existing ID and naming helpers. |
| The converter previously omits the displayed author. | `convertMessagesToChatHistory.types.ts::ChatMessage.authorName` | Carry the row's author name independently from the current composer selection. |
| `ChatMessageList.tsx` previously uses the composer name for every assistant row. | `ChatMessageList.tsx` | Use the persisted row name. Keep the composer fallback only when the row states no author. |
| `pages/chat/index.tsx::findActiveParticipantById` previously compares stored and wire IDs with strict equality. | The same helper | Match numeric and string IDs while retaining the allowed participant-kind guard. |
| Fresh creation previously announces the conversation without the attached active participant ID. | `useAddEntityParticipant.helpers.ts::applyParticipantSelection` and `CreatedConversation` | Attach the participant first. Announce the new conversation with its supported active participant ID. |
| The page owns `useLocalActiveParticipant` and the existing `ActiveConversationParticipantKey`. | `ChatBox.props.ts::onCreated` and `pages/chat/index.tsx::handleConversationCreated` | Save the selected ID through the existing owner before route navigation. |

Web paths after the first row refer to `apps/elitea-web/src/`.
An unresolved stated author uses the existing safe `Elitea` fallback.
It does not borrow another pipeline's composer name.
Unsupported toolkit participants cannot become the restored active participant.
The change introduces no storage key, backend route, database field, or runtime authority.

## Recorded deployed behavior

The saved chat838 live view shows `Gate5 NATS delayed refusal 20261007` as the answer author.
Normal reload shows `Elitea` and the default model composer.
The user message still names the selected pipeline.
The safe message, `PIPELINE_CODE_FAILED` code, and support reference remain unchanged.
The support reference is `e4eb1bd7-b45d-592e-b81a-6c571df98ba9`.
The persisted Main row retains author participant135.
These records expose the historical author projection defect.

Source checks also establish the strict ID lookup defect and the fresh-chat selection persistence gap.
The saved chat838 evidence does not capture its local participant storage.
It cannot identify which selection-loss branch occurs in that chat.
The default model composer alone does not prove a model call.
These UI findings do not change the accepted terminal failure or journal evidence.
The refused R19 capture does not become a successful recovery proof.

## Implementation history and verification

2026-10-07: Root reviews the core projection and ID patch and the separate fresh-chat creation patch.
The implementation changes eight Web production files and five test files.
The creation callback preserves attachment-before-navigation ordering.
A five-line value helper preserves the existing lint complexity limit.

The complete fresh-chat regression starts at `/chat` and selects a saved pipeline.
It verifies the allocated chat route and the selected participant in existing storage.
It unmounts the page and remounts that route.
It verifies the original composer participant and exactly one conversation creation.
The fixture uses the production `participantType` tag and the existing editor-width setup.
It restores the selected-project state and supplies the normal empty trace metadata response.

Two-pipeline regressions retain each answer's own author when the composer selection changes.
They cover crossed numeric and string IDs, unresolved authors, and rows without author metadata.
They retain the typed failure code, safe message, and support reference.
The selected-participant regression retains the supported-kind guard and rejects missing IDs.

| Local check | Direct status | Result |
| --- | ---: | --- |
| Six focused converter, message-list, page, helper, and hook suites | 0 | 76 unique tests pass; zero fail; zero skip. |
| `npm run typecheck` | 0 | TypeScript passes. |
| `npm run lint` | 0 | Existing lint rules pass. |
| `git diff --check` on the 13 owned Web files | 0 | Whitespace checks pass. |

Eight unique regression cases are added. Diagnostic reruns do not increase the final unique count.
Earlier fixture and lint failures remain preserved in the private implementation packet.
Existing jsdom and setup warnings remain recorded.
The final patch SHA-256 is `8179380e07279ac1d783d9ed4fcce092ae9ddba3c98ef4aba58ff3c1cef60588`.
The local result SHA-256 is `f14e8f26bebe075b8665451cff849b70341411bfe16c679af1d19ce85a43476e`.

## Deployment and connection limits

The deployed Main and Web source remains `efa7213e803d6f314ee8b6c482b78f45544140b4`.
The deployed Web image remains `sha256:1c343b749892ac042f5389e4018432e21776d97a902e9b0eb7e8fc18adb91714`.
The native Worker and Supervisor use source `c53ab7d5496a571c1844e9906a83c261ff0da06e`.
The local Web correction has no new image, deployment, real browser, or CI result.
These unit fixtures do not prove a deployed correction.

The sidebar dot reports the aggregate browser SSE status, not Worker health.
All registered channels offline produces Offline. An open channel produces Connected.
Reload remounts notification SSE and resets its retry state.
A successful open can therefore change Offline to Connected.
No saved notification status or browser online state identifies the actual incident cause.
No browser connection-limit cause is established. The connection policy remains unchanged.
The correction adds no Code runtime, capacity, recovery, or Kubernetes completion claim.
