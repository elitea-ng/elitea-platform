/**
 * Folder domain type — groups conversations in the chat sidebar. No OpenAPI
 * schema exists for this resource; shape derived from old-app evidence only:
 *
 * - apps/elitea-ui/src/[fsd]/features/chat/conversation-list/api/
 *   conversationList.api.js:22-137 (folderCreate/foldersList/folderUpdate/
 *   deleteFolder/folderPinUpdate — PATCH body `{ is_pinned }`).
 * - apps/elitea-ui/src/common/constants.js:99 — `DefaultFolderName = 'New folder'`.
 * - apps/elitea-ui/src/[fsd]/features/chat/conversation-list/lib/hooks/
 *   useQueryFoldersList.hooks.js:53-166 — the grouped-list envelope shape
 *   `{ pinned: { conversations }, date_groups: [{ name, conversations }],
 *   folders: [{ id, conversations }], selected_conversation_id, total_folders }`.
 *
 * `ConversationRef` is declared inline rather than importing entities/
 * conversation, per the dependency-cruiser `no-sideways-entities` rule.
 */

export const DEFAULT_FOLDER_NAME = 'New folder';

export interface FolderConversationRef {
  readonly id: string;
  readonly name?: string;
  readonly isPrivate?: boolean;
  readonly updatedAt?: string;
  readonly createdAt?: string;
  /**
   * Owner of the conversation. The grouped-list wire carries it as
   * `author_id`, an integer. The sidebar row menu needs it. The menu disables
   * Delete and Edit on another member's conversation. The normaliser dropped
   * this field before, so that check compared two `undefined` values.
   */
  readonly authorId?: string | undefined;
  /** Needed to compute `genConversationId` parity — see `lib/normalise.ts`. */
  readonly isPlayback?: boolean;
}

export interface Folder {
  readonly id: string;
  readonly name: string;
  readonly conversations: readonly FolderConversationRef[];
  readonly total?: number;
  readonly offset?: number;
  readonly isPinned?: boolean;
  /**
   * Client-only: not-yet-persisted folder, synthetic id, not a wire field.
   * apps/elitea-ui/src/[fsd]/features/chat/conversation-list/lib/hooks/
   * useMoveToFolderConversation.hooks.js:229-238 builds exactly this shape
   * (`{id, name, conversations, isNew: true, targetConversationId,
   * targetConversation}`) for the drag-to-"new folder" flow;
   * `targetConversationId`/`targetConversation` are not modeled here as
   * they are transient hook-local wiring, not persisted folder state.
   */
  readonly isNew?: boolean;
}

/**
 * One `date_groups[]` bucket from the grouped folders-list envelope.
 *
 * `total`/`offset` are the SAME pair `Folder` above already models, and they
 * are optional for the same reason: a bucket that is served whole (a filtered
 * listing) omits them. They were missing here while the wire carried them,
 * which made the rail's load-more sentinel unreachable — the sentinel mounts
 * only while `total` exceeds the rows a bucket holds, so a bucket that never
 * learns its own total can never report a remainder. `total` is the size of
 * the WHOLE bucket, `offset` is where its next page starts.
 */
export interface DateGroup {
  readonly name: string;
  readonly conversations: readonly FolderConversationRef[];
  readonly total?: number;
  readonly offset?: number;
}

/** The full `?grouped=true` response envelope — see the module doc citation. */
export interface GroupedFoldersResponse {
  readonly pinned: { readonly conversations: readonly FolderConversationRef[] };
  readonly dateGroups: readonly DateGroup[];
  readonly folders: readonly Folder[];
  readonly selectedConversationId?: string;
  readonly totalFolders: number;
}
