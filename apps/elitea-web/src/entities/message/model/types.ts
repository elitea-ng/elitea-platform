/**
 * Message domain type — a chat message, both in its persisted
 * (`message_group`) form and as a live socket-streamed payload. No OpenAPI
 * schema exists for this resource (P6/§5.5: socket.io is the second API and
 * is not spec-described).
 *
 * `SocketMessageType` and `RawSocketMessage` are TYPE-ONLY re-derivations of
 * the socket contract (`shared/api/socket/messages.ts`)
 * rather than a second, hand-maintained parallel
 * definition: `import type` + `typeof` erase completely at compile time (no
 * runtime edge into `shared/api/socket`, verified — `tsc --noEmit` on a
 * throwaway probe file emits zero output), and deriving structurally means
 * this type can never drift from S5's again, by construction. An earlier
 * version of this file hand-duplicated the shape ahead of S5 landing and DID
 * drift on delivery — S5's `chat_user_message`/`mcp_authorization_required`/
 * `agent_llm_chunk`/`chat_predict_summary_started`/`swarm_child_message`
 * variants each carry real per-discriminant sibling fields
 * (`author_participant_id`/`uuid`/`sent_to_id`/`message_items`,
 * `stream_id`, `thinking`, `payload`, `parent_message_id`/`agent_name`) that
 * the hand-written version omitted or mis-scoped as universal fields; S5
 * also confirmed `response_metadata` is fully opaque (not the narrower
 * `{metadata:{thread_id}}` shape previously asserted) and `created_at` is
 * `string | number`, not `string`-only. See `shared/api/socket/messages.ts`
 * for the full per-discriminant evidence citations (constants.js line refs
 * + old-app hook call sites) — they are not repeated here to avoid a SECOND
 * copy that can itself go stale.
 *
 * Evidence for the POST-normalisation `Message`/`UserMessage`/
 * `AssistantMessage` types below (a different data source — persisted
 * `message_group` DB rows, not live streaming payloads):
 * - apps/elitea-ui/src/common/convertChatConversationMessages.js:35-313 —
 *   the persisted message_group -> UI message normaliser
 *   (`convertToUserQuestion`/`convertToAIAnswer`), which is the shape
 *   `lib/normalise.ts` in this slice ports.
 */
import type { z } from 'zod';

import type { SOCKET_MESSAGE_TYPES, socketMessageSchema } from '@/shared/api/socket/messages';

/**
 * The 34 streaming payload discriminants — derived from S5's
 * `SOCKET_MESSAGE_TYPES` (constants.js declaration order), not
 * hand-duplicated. NOT the 43 channel event names (those are
 * `entities/canvas`'s and S5's `events.ts` concern) — this is the `type`
 * field carried inside a socket message payload.
 */
export type SocketMessageType = (typeof SOCKET_MESSAGE_TYPES)[number];

/**
 * A raw socket message envelope as received off the wire, PRE-normalisation
 * — the exact union S5's `socketMessageSchema` validates against, including
 * every per-discriminant sibling field (see the module doc above).
 */
export type RawSocketMessage = z.infer<typeof socketMessageSchema>;

/** One entry of a message's `toolActions[]` (thinking-step / tool-call / swarm-child union). */
export interface ToolAction {
  readonly type: string;
  readonly timestamp?: string;
  readonly [key: string]: unknown;
}

interface MessageBase {
  readonly id: string;
  readonly content: string;
  /** `message_group.message_items[]` — attachments, canvas blocks, etc. */
  readonly messageItems?: readonly unknown[];
  readonly createdAt?: string;
}

/**
 * `convertToUserQuestion` (convertChatConversationMessages.js:35-82).
 */
export interface UserMessage extends MessageBase {
  readonly role: 'user';
  readonly name?: string;
  readonly avatar?: string;
  readonly userId?: string;
  readonly participantId?: string;
  readonly sentTo?: unknown;
  readonly likes?: number;
  readonly interactionUuid?: string;
}

/**
 * `convertToAIAnswer` (convertChatConversationMessages.js:111-313).
 */
export interface AssistantMessage extends MessageBase {
  readonly role: 'assistant';
  readonly updatedAt?: string;
  readonly replyToId?: string;
  readonly questionId?: string;
  readonly taskId?: string;
  readonly isStreaming?: boolean;
  readonly isLoading?: boolean;
  readonly isSummarized?: boolean;
  readonly exception?: unknown;
  readonly references?: unknown;
  readonly toolActions?: readonly ToolAction[];
  readonly hitlInterrupt?: unknown;
  readonly hitlInterrupts?: readonly unknown[];
  readonly threadId?: string;
  readonly requiresConfirmation?: { readonly message: string; readonly buttonText: string };
  readonly likes?: number;
  /**
   * How many of the caller's persistent, cross-conversation memories (#870)
   * this turn's recall used. `undefined` (not `0`) whenever the turn used
   * none — see `MessageGroupMetaWire.memories_used`'s own comment.
   */
  readonly memoriesUsed?: number;
}

export type Message = UserMessage | AssistantMessage;
