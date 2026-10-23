/**
 * Shared state-tree shapes for this directory's 9 hooks — not one of the 8
 * enumerated hook files itself, but a small extraction to avoid the same
 * ~15-line interface being redeclared in every one of them (the "avoid
 * duplicated logic" spirit the C2 brief applies elsewhere). No baseline
 * equivalent: the old app's hooks are untyped JS operating on whatever
 * shape the caller happened to hold in React state.
 *
 * **Judgment call, disclosed:** the baseline's `folders`/`dateGroups`/
 * `pinnedConversations` arrays hold FULL conversation records (`name`,
 * `folder_id`, `isPinned`, timestamps, …) — richer than `entities/folder`'s
 * own `FolderConversationRef`, which is deliberately minimal (that type
 * models exactly what the folders-LIST wire response gives per its own
 * module doc: `id`/`updatedAt`/`createdAt`/`isPlayback` only). `entities/
 * conversation`'s `Conversation` is a structural superset of
 * `FolderConversationRef` (same 4 fields, plus everything else these hooks'
 * callers actually need — `name`, `folderId`, `isPinned`, …), so every
 * conversations-array element in THIS feature's state trees is typed as
 * `Conversation` rather than `FolderConversationRef`.
 */
import type { Conversation } from '@/entities/conversation';
import type { Folder } from '@/entities/folder';

/**
 * A `Folder` whose `conversations` are full `Conversation` records (see
 * module doc) instead of bare refs, plus 3 fields that exist ONLY as
 * drag-and-drop-computed, about-to-be-PUT scratch state
 * (`useDragAndDrop.ts`/`useReorderFolders.ts`) — never part of the
 * persisted, normalised `Folder` domain type (`entities/folder/api/
 * foldersApi.ts`'s `folderUpdate` accepts them as loose extra body keys,
 * per its own `FolderUpdateParams` `[key: string]: unknown` index
 * signature). Left snake_case (unlike every OTHER field here) because they
 * are sent to `folderApi.update`'s JSON body verbatim, with no
 * wire-normalisation layer of their own to translate through — baseline:
 * `hooks/chat/useDragAndDrop.js`'s own `neighbor_above_id`/
 * `neighbor_below_id`/`position` fields.
 */
export interface FolderListItem extends Omit<Folder, 'conversations'> {
  readonly conversations: readonly Conversation[];
  /**
   * NOT modelled on `entities/folder`'s `Folder` at all — no real folder
   * producer (`folderApi.list`/`.create`) ever sets it. Preserved here only
   * because `useDeleteFolder.hooks.ts`'s baseline
   * (`hooks/chat/useDeleteFolder.js:19`) guards its network call on exactly
   * this field, and this port keeps that guard byte-faithful even though it
   * is (today) unreachable for any real folder — see that hook's own doc
   * comment.
   */
  readonly isPlayback?: boolean;
  readonly position?: number;
  readonly neighbor_above_id?: string | number | null;
  readonly neighbor_below_id?: string | number | null;
}

/**
 * The synthetic, not-yet-persisted "move conversation to a new folder"
 * draft folder — baseline: `useMoveToFolderConversation.hooks.js:229-238`
 * builds exactly `{id, name, conversations: [], isNew: true,
 * targetConversationId, targetConversation}`. `entities/folder`'s
 * `Folder.isNew` doc comment explicitly excludes `targetConversationId`/
 * `targetConversation` as "transient hook-local wiring, not persisted
 * folder state" — this is that hook-local extension.
 */
export interface NewFolderDraft extends FolderListItem {
  readonly isNew: true;
  readonly targetConversationId: string;
  readonly targetConversation: Conversation;
}

/** A `DateGroup`-shaped bucket (`entities/folder`'s own `DateGroup`, minus its `FolderConversationRef`-typed `conversations` — see module doc) with the `total`/`offset` baseline pagination fields `useQueryFoldersList.hooks.js:79-86`'s `updateDateGroups` attaches locally (not part of the wire `DateGroup` shape). */
export interface DateGroupListItem {
  readonly name: string;
  readonly conversations: readonly Conversation[];
  readonly total?: number;
  readonly offset?: number;
  /** Client contract 1.12: how many conversations of the whole bucket need the person (the listing's count). */
  readonly needsYou?: number;
}
