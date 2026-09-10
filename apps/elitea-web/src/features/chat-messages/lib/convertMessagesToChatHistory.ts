/**
 * Ported from `apps/elitea-ui/src/common/convertChatConversationMessages.js`
 * (392 lines) — the full message-group → chat-history converter used by
 * a transcript pager (injected parameter, deviation
 * #3) and by playback mode (`PlaybackChatBox` / `PlaybackToolBar`).
 *
 * `entities/message/lib/normalise.ts` already ports `convertTime`,
 * `normaliseUserMessage` and `normaliseAssistantMessage` (per-message-group
 * bodies of `convertToUserQuestion` / `convertToAIAnswer`). This slice owns
 * the list-level concerns the entity normaliser explicitly left out:
 * chronological sort, parent/child-row split, swarm-child `toolActions`
 * attachment — lines 315-387 of the source.
 *
 * Also ports `convertToPlayerQuestion` (backs playback mode),
 * `isUserMessage` (helper used during conversation iteration), and
 * `convertConversationToChatHistory` (convenience wrapper).
 *
 * `collapseSubAgentInvocationKeys` from `entities/message/lib/subAgentGrouping`
 * is imported for the persisted-reload path; the live-streaming grouping
 * functions (`partitionActionsIntoBlocks`, etc.) are built locally in
 * `./subAgentGrouping.ts` for the streaming accordion view.
 *
 * Normalized trace summaries arrive separately from the message metadata.
 * The shared message reader attaches scoped `persisted_trace` references.
 * The answer carries these references to the lazy detail renderer.
 *
 * Parity notes:
 * - `isUserMessage` (lines 9-15): ported verbatim.
 * - `convertToPlayerQuestion` (lines 84-108): ported verbatim.
 * - `convertMessagesToChatHistory` (lines 315-387): chronological sort,
 *   parent/child-row split (swarm-child via `meta.is_child_agent`),
 *   swarm-child `toolActions` attachment (lines 351-381).
 * - `convertConversationToChatHistory` (lines 389-392): convenience wrapper.
 */
import { ROLES } from '@/shared/lib/enums';
import { ChatParticipantType, TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';

import type { MessageGroupWire, MessageItemWire, MessageParticipantWire } from '@/entities/message/lib/wire';
import { convertTime, isParticipant, normaliseAssistantMessage, normaliseUserMessage } from '@/entities/message/lib/normalise';
import type { SubAgentGroupable } from '@/entities/message/lib/subAgentGrouping';

import { splitWholeResponse } from './chatStreamReasoning';
import type { ToolAction } from './chatStreamToolAction';

// ---------------------------------------------------------------------------
// ChatMessage union type
// ---------------------------------------------------------------------------

/**
 * Unified chat message returned by `convertMessagesToChatHistory`.
 * Combines the `UserMessage` and `AssistantMessage` entity types plus
 * list-level fields (`questionId`, `replyToId`, `originalId`) that
 * `convertMessagesToChatHistory` adds at the conversation level.
 */
export interface ChatMessage {
  readonly persistedTrace?: unknown;
  readonly id: string;
  readonly role: string;
  readonly name: string;
  readonly avatar?: string | undefined;
  readonly content: string;
  readonly createdAt: string;
  readonly messageItems?: readonly MessageItemWire[] | undefined;
  readonly userId?: string | undefined;
  readonly participantId?: string | undefined;
  readonly sentTo?: unknown;
  readonly likes?: number | undefined;
  readonly interactionUuid?: string | undefined;
  readonly toolActions?: readonly SubAgentGroupable[] | undefined;
  readonly exception?: unknown;
  readonly isStreaming?: boolean | undefined;
  readonly isLoading?: boolean | undefined;
  /**
   * Set alongside `isStreaming`/`isLoading` by the stream reducer's interrupt
   * slice. A pause has to CLEAR it: `isMessageInFlight` treats it as in-flight,
   * so a message that paused mid-regenerate would keep the composer disabled
   * and suppress the live thinking view that hosts the approval card.
   */
  readonly isRegenerating?: boolean | undefined;
  readonly questionId?: string | undefined;
  readonly replyToId?: string | undefined;
  readonly references?: readonly unknown[] | undefined;
  readonly isSummarized?: boolean | undefined;
  readonly hitlInterrupt?: unknown;
  readonly hitlInterrupts?: readonly unknown[] | undefined;
  /**
   * The continue-button prompt shown when a turn stopped on the model's token
   * limit rather than on a completed answer. `ApplicationAnswer` reads it to
   * decide whether to render `ChatContinue`.
   */
  readonly requiresConfirmation?: { readonly message: string; readonly buttonText: string } | undefined;
  readonly threadId?: string | undefined;
  readonly taskId?: string | undefined;
  readonly originalId?: string | number | undefined;
  /** How many persistent memories (#870) this turn's recall used — see `AssistantMessage.memoriesUsed`'s own comment. */
  readonly memoriesUsed?: number | undefined;
}

// ---------------------------------------------------------------------------
// isUserMessage (source: convertChatConversationMessages.js:9-15)
// ---------------------------------------------------------------------------

/**
 * `isUserMessage` — determines whether a `message_group` represents a user
 * message by checking author, sender, and reply-to relationships against
 * the known user participant ids.
 */
export function isUserMessage(
  authorParticipantId: string | number | undefined,
  sentToId: string | number | undefined,
  userIds: readonly (string | number)[],
  replyToId: string | number | undefined,
  sentTo: { entity_name?: string } | undefined,
  /**
   * The row's own `role`, when the source states one.
   *
   * `GET /elitea_core/messages/prompt_lib/{project}/{conversation}` — what a
   * conversation deep link reads — answers rows shaped
   * `{id, uid, role, content}` with NO `author_participant_id`, `sent_to_id`
   * or `reply_to_id`. Every clause of the heuristic below then misses except
   * `(!sentToId && !replyToId)`, which is TRUE, so an assistant reply was
   * classified as a USER message: on reload the answer rendered as a second
   * question, authored by "User No Longer Available" (measured). An explicit
   * role is authoritative and needs no inference.
   */
  role?: string,
): boolean {
  if (role === 'user') return true;
  if (role === 'assistant') return false;
  // The membership tests are the same participant lookup in `includes` form,
  // and `includes` is strict: a Go row stating `author_participant_id: 1`
  // against a roster of string ids matched nothing, and a question whose
  // `sent_to_id` is set (so the `(!sentToId && !replyToId)` clause is false)
  // was then classified as an ASSISTANT answer and normalised as one.
  // `?? ''` is dropped with it: an id the row never states must not match a
  // roster entry that happens to be empty.
  return (
    userIds.some((id) => isParticipant(id, authorParticipantId)) ||
    userIds.some((id) => isParticipant(id, sentToId)) ||
    (!sentToId && !replyToId) ||
    !!sentTo
  );
}

// ---------------------------------------------------------------------------
// PlayerInfo (source: convertChatConversationMessages.js:84)
// ---------------------------------------------------------------------------

/** Player info shape accepted by `convertToPlayerQuestion`. */
export interface PlayerInfo {
  readonly user: { readonly name?: string | undefined; readonly avatar?: string | undefined };
  readonly firstUserMessage?: { readonly author_participant_id?: string | number };
}

// ---------------------------------------------------------------------------
// convertToPlayerQuestion (source: convertChatConversationMessages.js:84-108)
// ---------------------------------------------------------------------------

/**
 * `convertToPlayerQuestion` — converts a message group to a user question
 * suitable for playback mode, using player-specific name/avatar resolution.
 */
// eslint-disable-next-line eslint/complexity — playback conversion has many branching paths
export function convertToPlayerQuestion(
  messageGroup: MessageGroupWire,
  playerInfo: PlayerInfo,
  participants: readonly MessageParticipantWire[],
): ChatMessage {
  const { content, message_items, created_at, uuid, sent_to_id, author_participant_id, likes } = messageGroup;
  // `isParticipant`, not `===`: the ids cross this wire in two spellings (see
  // its docblock). One lookup governs `name` and `avatar` below, the other
  // `sentTo` — and this function already stringifies `sent_to_id` for
  // `participantId`, so comparing it strictly was an inconsistency inside one
  // returned object.
  const sentToParticipant = participants.find((p) => isParticipant(p.id, sent_to_id));
  const authorParticipant = participants.find((p) => isParticipant(p.id, author_participant_id));

  // The wire type uses snake_case (`user_name`, `user_avatar`); the old app
  // also reads these same properties from its Participant shape.
  let name = authorParticipant?.meta?.user_name ?? 'You';
  let avatar = authorParticipant?.meta?.user_avatar ?? '';
  // Declared `string | number` on one side and `string` on the other, compared
  // strictly — the mismatch that decides whether the player's own name and
  // avatar are used at all. `undefined` on either side no longer matches: a
  // `playerInfo` carrying no `firstUserMessage` used to compare equal to an
  // author who did not resolve, and captioned an anonymous row as the player.
  if (isParticipant(authorParticipant?.id, playerInfo.firstUserMessage?.author_participant_id)) {
    name = playerInfo.user.name ?? name;
    avatar = playerInfo.user.avatar ?? avatar;
  }

  const sortedItems = [...(message_items ?? [])].sort((a, b) => (a.id ?? 0) - (b.id ?? 0));

  return {
    id: uuid,
    role: ROLES.User,
    name,
    avatar,
    content,
    messageItems: sortedItems,
    createdAt: convertTime(created_at),
    participantId: sent_to_id === undefined ? undefined : String(sent_to_id),
    sentTo: sentToParticipant,
    likes,
  };
}

// ---------------------------------------------------------------------------
// SwarmChild tool action (source: convertChatConversationMessages.js:351-381)
// ---------------------------------------------------------------------------

/**
 * A swarm-child tool action embedded in an AI answer's `toolActions[]`.
 * Matches the old-app shape exactly (field names preserved for the renderer).
 */
function buildSwarmChildAction(child: MessageGroupWire): Record<string, unknown> {
  const meta = child.meta as Record<string, unknown> | undefined;
  // Get text content from message_items — API uses 'text_message' as item_type.
  const textItem = child.message_items?.find(
    (item) => (item as unknown as Record<string, unknown>).item_type === 'text_message',
  );
  const content = textItem?.item_details?.content ?? child.content ?? '';

  return {
    id: child.uuid,
    name: meta?.child_agent_name ?? 'Child Agent',
    type: TOOL_ACTION_TYPES.SwarmChild,
    status: ToolActionStatus.complete,
    content,
    toolInputs: '',
    toolOutputs: content,
    created_at: child.created_at,
    ended_at: child.created_at,
    timestamp: child.created_at,
    isSwarmChild: true,
    agentName: meta?.child_agent_name ?? 'Child Agent',
  };
}

// ---------------------------------------------------------------------------
// convertMessagesToChatHistory (source: convertChatConversationMessages.js:315-387)
// ---------------------------------------------------------------------------

/**
 * `convertMessagesToChatHistory` — converts a list of message groups (from the
 * persisted conversation) into a chat-history array of `ChatMessage` objects.
 *
 * Implements:
 *  1. Chronological sort by `created_at`
 *  2. Parent/child-row split (swarm-child detection via `meta.is_child_agent`)
 *  3. Per-message-group normalisation via `normaliseUserMessage` /
 *     `normaliseAssistantMessage`
 *  4. Swarm-child `toolActions` attachment (lines 315-387)
 *
 * The result is the shape a transcript pager would prepend onto the running
 * chat history; the one that existed had no caller and was removed (issue
 * 852), so this function's only consumers today are the initial seed in
 * `widgets/chat-box` and the playback loader.
 */
// eslint-disable-next-line eslint/complexity — full conversion pipeline with parent/child/swarm branching
/**
 * Do to a STORED answer what the reducer does to a streamed one.
 *
 * A reasoning model's monologue is part of the answer text the backend
 * persists — measured: the stored content of a Qwen3.5 turn opens with
 * "Thinking Process:" and carries a bare `</think>` between the monologue and
 * the reply. `chatStreamReasoning` peels that off while the turn streams, so
 * the bubble reads correctly right up until the page is reloaded, at which
 * point the same answer comes back through THIS function and the whole
 * monologue reappears. Two renderings of one message is a worse defect than
 * either one alone, because only one of them is ever seen at a time.
 *
 * `splitWholeResponse` is the same call the reducer's whole-response frame
 * makes, so the row a reload produces is the row the live turn produced.
 */
function splitPersistedReasoning(message: ChatMessage): ChatMessage {
  const { answer, actions } = splitWholeResponse(
    message.id,
    message.content,
    (message.toolActions ?? []) as readonly ToolAction[],
    message.createdAt,
  );
  if (answer === message.content) return message;
  return { ...message, content: answer, toolActions: actions as unknown as readonly SubAgentGroupable[] };
}

export function convertMessagesToChatHistory(
  messageGroups: readonly MessageGroupWire[] = [],
  participants: readonly MessageParticipantWire[] = [],
  playerInfo?: PlayerInfo,
): readonly ChatMessage[] {
  const sortedMessages = [...(messageGroups ?? [])].sort((a, b) =>
    (a.created_at ?? '').toLowerCase().localeCompare((b.created_at ?? '').toLowerCase()),
  );

  // Users are participants with entity_name === ChatParticipantType.Users.
  // The old app filters on `entity_name` (a field on the participants wire
  // shape that carries the full participant including entity_name).
  // In the new app, participants come from entities/participant and use
  // a different shape. We fall back to checking meta fields.
  const users =
    participants?.filter(
      (p) =>
        // eslint-disable-next-line @typescript-eslint/no-unsafe-member-access -- runtime shape may vary
        (p as unknown as Record<string, unknown>).entity_name === ChatParticipantType.Users ||
        p.meta?.user_name !== undefined ||
        p.meta?.user_avatar !== undefined,
    ) ?? [];
  const userIds = users.map((u) => u.id);

  // Separate child messages from parent messages (swarm-mode children).
  const childMessagesByParent: Record<string, MessageGroupWire[]> = {};
  const parentMessages: MessageGroupWire[] = [];

  for (const mg of sortedMessages) {
    const meta = mg.meta as Record<string, unknown> | undefined;
    const isChildAgent = meta?.is_child_agent === true || meta?.is_child_agent === 'true';
    const parentMessageId = meta?.parent_message_id as string | undefined;

    if (isChildAgent && parentMessageId) {
      if (!childMessagesByParent[parentMessageId]) {
        childMessagesByParent[parentMessageId] = [];
      }
      childMessagesByParent[parentMessageId].push(mg);
    } else {
      parentMessages.push(mg);
    }
  }

  // Convert parent messages and attach swarm-child toolActions.
  return parentMessages.map((messageGroup) => {
    const { author_participant_id, sent_to_id, reply_to_id, sent_to, uuid } = messageGroup;
    const isUser = isUserMessage(
      author_participant_id,
      sent_to_id,
      userIds,
      reply_to_id,
      sent_to,
      (messageGroup as { role?: string }).role,
    );

    if (isUser) {
      if (playerInfo) {
        return convertToPlayerQuestion(messageGroup, playerInfo, participants);
      }
      // normaliseUserMessage returns a UserMessage; cast to ChatMessage.
      return normaliseUserMessage(messageGroup, users, participants) as ChatMessage;
    }

    // Convert AI answer using entities-level normaliser.
    const aiMessage = splitPersistedReasoning(
      normaliseAssistantMessage(messageGroup, sortedMessages, participants) as unknown as ChatMessage,
    );

    if (messageGroup.persisted_trace) Object.assign(aiMessage, { persistedTrace: messageGroup.persisted_trace });

    // Attach child messages as SwarmChild toolActions.
    const childMessages = childMessagesByParent[uuid] ?? [];
    if (childMessages.length > 0) {
      const swarmChildActions: SubAgentGroupable[] = childMessages.map((child) => buildSwarmChildAction(child) as unknown as SubAgentGroupable);

      // Prepend SwarmChild actions to toolActions (they appear before other tools).
      const existingActions = aiMessage.toolActions ?? [];
      return { ...aiMessage, toolActions: [...swarmChildActions, ...existingActions] };
    }

    return aiMessage;
  });
}

// ---------------------------------------------------------------------------
// convertConversationToChatHistory (source: convertChatConversationMessages.js:389-392)
// ---------------------------------------------------------------------------

/** Conversation details shape accepted by `convertConversationToChatHistory`. */
export interface ConversationDetails {
  readonly message_groups?: readonly MessageGroupWire[];
  readonly participants?: readonly MessageParticipantWire[];
}

/**
 * `convertConversationToChatHistory` — convenience wrapper that extracts
 * `message_groups` and `participants` from a conversation details object.
 */
export function convertConversationToChatHistory(
  conversationDetails: ConversationDetails = {},
  playerInfo?: PlayerInfo,
): readonly ChatMessage[] {
  const { message_groups = [], participants = [] } = conversationDetails;
  return convertMessagesToChatHistory(message_groups, participants, playerInfo);
}
