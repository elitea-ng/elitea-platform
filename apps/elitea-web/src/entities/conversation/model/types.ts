/**
 * Conversation domain type — a chat thread. No OpenAPI schema exists for
 * this resource (chat-domain, not in the W2 manifest); shape derived from
 * old-app evidence only, flagged per unit E1 instructions:
 *
 * - apps/elitea-ui/src/[fsd]/features/chat/api/chat.api.js:100-245
 *   (conversationCreate/Edit/Details/regenerate endpoints).
 * - apps/elitea-ui/src/common/constants.js:1023 — default shape
 *   `dummyConversation = { name: '', chat_history: [], participants: [],
 *   is_private: true }`.
 * - apps/elitea-ui/src/[fsd]/features/chat/conversation-list/lib/helpers/
 *   conversationList.helpers.js:22-42 (sort fields `updated_at`/`created_at`).
 * - apps/elitea-ui/src/[fsd]/features/chat/conversation-list/lib/hooks/
 *   useMoveToFolderConversation.hooks.js:229-238 — the synthetic
 *   not-yet-persisted FOLDER object built when dragging a conversation to
 *   "new folder" (`{id, name, conversations, isNew: true,
 *   targetConversationId, targetConversation}`). NOTE: `isNew`/
 *   `targetConversationId`/`targetConversation` live on that synthetic
 *   FOLDER, not on the conversation being moved — see entities/folder's
 *   `Folder.isNew` for the correctly-scoped citation. `DraftConversation`
 *   below borrows the `isNew` NAME for a conversation-side analogue (a
 *   not-yet-persisted conversation, e.g. `dummyConversation`), but that is
 *   this module's own modeling choice, not a port of this hook's fields.
 *
 * `MessageGroup`/`Participant` are declared inline (mirroring
 * entities/message and entities/participant) rather than imported, per the
 * dependency-cruiser `no-sideways-entities` rule (entities may not import
 * one another).
 */

interface ConversationParticipantRef {
  readonly id: string;
  readonly entityName: string;
}

export interface Conversation {
  readonly id: string;
  readonly name: string;
  readonly folderId?: string;
  readonly isPrivate: boolean;
  readonly createdAt?: string;
  readonly updatedAt?: string;
  readonly chatHistory?: readonly unknown[];
  readonly participants?: readonly ConversationParticipantRef[];
  readonly total?: number;
  readonly offset?: number;
  /**
   * Owner of the conversation (`author_id` on the wire, normalised to a
   * string). The chat sidebar disables Delete and Edit on a conversation the
   * current user does not own, and that check needs this value.
   */
  readonly authorId?: string | undefined;
  /** Client-only: set by usePinConversation, not a wire field. */
  readonly isPinned?: boolean;
  /** Client-only: playback/replay mode flag, not a wire field. */
  readonly isPlayback?: boolean;
  /** Client-only: rename-in-progress flag, not a wire field. */
  readonly isNamingPending?: boolean;
  /** `chat_conversations.source` from the rail's listing — `local_work` marks a desktop Local work thread. */
  readonly source?: string;
}

/** `DefaultFolderName`/`dummyConversation`-equivalent — a not-yet-persisted conversation. */
export interface DraftConversation extends Partial<Conversation> {
  readonly isNew: true;
}
