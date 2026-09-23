import type { Dispatch, SetStateAction } from 'react';

import type { Conversation } from '@/entities/conversation';

import type { DateGroupListItem, FolderListItem } from '../../lib/hooks/conversationListState.types';
import type { UseMoveToFolderConversationResult } from '../../lib/hooks/useMoveToFolderConversation.hooks';
import type { FolderItemCallbacks, FolderMoveTargetCallbacks } from '../folders/FolderItem';

/**
 * `entities/folder`'s `Folder`/this feature's own `FolderListItem` don't
 * model an owner id or a "no more pages" flag — the SAME disclosed gap
 * `ui/folders/FolderItem.tsx`'s own `readFolderOwnerId` doc comment already
 * covers for `owner_id` (evidence: baseline `Folders.jsx`'s sibling
 * `FolderItem.jsx:561`). Declared here as a real, typed optional field
 * (rather than `FolderItem.tsx`'s permissive-cast reader) because
 * `Conversations.tsx`, unlike that file, owns the actual `folders` state
 * shape via its own `setFolders` prop, so a future composition-root unit can
 * just populate it directly. `exhausted` is this unit's own load-more
 * bookkeeping (baseline: `Conversations.jsx:167`'s `g.exhausted`/
 * `f.exhausted`), never a wire field either.
 */
export interface ConversationsFolder extends FolderListItem {
  readonly exhausted?: boolean | undefined;
  readonly ownerId?: string | undefined;
}

/** Same class of local addition as `ConversationsFolder.exhausted` above — baseline: `Conversations.jsx:167`'s `g.exhausted`. */
export interface ConversationsDateGroup extends DateGroupListItem {
  readonly exhausted?: boolean | undefined;
}

/**
 * `Conversations.tsx`'s full prop list — split into its own file purely to
 * keep that file under the §3.5 `max-lines` budget (400); a pure type
 * declaration has no runtime behaviour to keep near its component.
 * Port of `Conversations.jsx:37-77`'s ~35 props faithfully, plus this port's
 * own disclosed additions (`projectId`/`currentUserId`/`personalProjectId`/
 * `publicProjectId`/`isEditingCanvas`/`basename`/`onShareLinkCopied`/
 * `sortBy`/`sortOrder`) — see `Conversations.tsx`'s own module doc for the
 * full rationale.
 */
export interface ConversationsProps {
  readonly conversations: readonly Conversation[];
  readonly pinnedConversations: readonly Conversation[];
  readonly dateGroups: readonly ConversationsDateGroup[];
  readonly setDateGroups: Dispatch<SetStateAction<readonly ConversationsDateGroup[]>>;
  readonly ungroupedConversationsCount: number;
  readonly totalConversationsAmount: number;
  readonly onSelectConversation: (conversation: Conversation) => void;
  readonly selectedConversationId?: string | undefined;
  readonly collapsed?: boolean | undefined;
  readonly onCollapsed: () => void;
  readonly onEditConversation: (conversation: Conversation) => void;
  readonly onPlaybackConversation: (conversation: Conversation) => void;
  readonly onDeleteConversation: (conversation: Conversation) => void;
  /** Baseline dead-prop pass-through into `Folders` (`Folders.jsx` never used either meaningfully) — kept on this interface for a faithful full port of `Conversations.jsx:37-77`'s prop list, deliberately never read in `Conversations.tsx`'s body (this unit's brief: don't reproduce that pass-through, since the `Folders`-shaped middle tier it fed no longer exists). */
  readonly onLoadMore?: (() => void) | undefined;
  readonly isLoadConversations?: boolean | undefined;
  /** See `onLoadMore` above — same disclosed dead-prop class. */
  readonly isLoadMoreConversations?: boolean | undefined;
  readonly onPinConversation: (conversation: Conversation, shouldPin: boolean) => void;
  /** Issue 940/A6 — clones a conversation (participants + settings) into a new, independent one and selects it. */
  readonly onDuplicateConversation: (conversation: Conversation) => void;
  readonly onCreateConversation: (conversation: Conversation) => Promise<unknown>;
  readonly onCancelCreateConversation: () => void;
  readonly onChangeActiveConversationName: (name: string) => void;
  readonly onChangeActiveFolderName?: FolderItemCallbacks['onChangeActiveFolderName'];
  readonly onCreateFolder: FolderItemCallbacks['onCreateFolder'];
  readonly onCancelCreateFolder: FolderItemCallbacks['onCancelCreateFolder'];
  readonly folders: readonly ConversationsFolder[];
  readonly setFolders: Dispatch<SetStateAction<readonly ConversationsFolder[]>>;
  readonly onDeleteFolder: FolderItemCallbacks['onDeleteFolder'];
  readonly onEditFolder: FolderItemCallbacks['onEditFolder'];
  readonly onPinFolder: FolderItemCallbacks['onPinFolder'];
  readonly onMoveToFolderConversation: UseMoveToFolderConversationResult['onMoveToFolderConversation'];
  readonly onMoveToNewFolderConversation: UseMoveToFolderConversationResult['onMoveToNewFolderConversation'];
  readonly moveTargetConversationToNewFolder: FolderMoveTargetCallbacks['moveTargetConversationToNewFolder'];
  readonly cancelMovingTargetConversationToNewFolder: FolderMoveTargetCallbacks['cancelMovingTargetConversationToNewFolder'];
  readonly onClickCreateNewFolder: () => void;
  readonly enableDragAndDrop?: boolean | undefined;
  readonly toastSuccess?: ((message: string) => void) | undefined;
  readonly toastError: (message: string) => void;
  readonly onReorderFolders?: ((newOrder: readonly FolderListItem[]) => Promise<void>) | undefined;
  readonly isFolderOperationInProgress?: boolean | undefined;
  readonly onSearchQueryChange?: ((query: string | undefined) => void) | undefined;
  /** N4 signature deviation — needed for the load-more fetches, the "Move to"-menu permission checks, and threaded into `ConversationItem`/`FolderItem`. */
  readonly projectId?: string | undefined;
  /** Baseline: `const { id: userId } = useSelector(state => state.user);` (`Conversations.jsx:82`) — same explicit-prop substitution instruction as `ConversationItem.tsx`'s own `currentUserId` (this is the SAME value, threaded to both). */
  readonly currentUserId?: string | undefined;
  readonly personalProjectId?: string | number | undefined;
  readonly publicProjectId?: string | number | undefined;
  /** Baseline: `const { isEditingCanvas } = useSelector(state => state.settings.navBlocker);` (`Conversations.jsx:89`) — plain prop, same substitution instruction as `ConversationItem.tsx`'s own `isEditingCanvas`. */
  readonly isEditingCanvas?: boolean | undefined;
  /** Threaded straight through to every `ConversationItem` this file builds — see that component's own doc comment. */
  readonly basename?: string | undefined;
  readonly onShareLinkCopied?: (() => void) | undefined;
  /**
   * N4 signature deviation replacing the baseline's `useSortQueryParamsFromUrl`
   * (a router-URL read `features/` may not perform directly, R-L1) — same
   * substitution, same defaults, `useQueryFoldersList.hooks.ts`'s own
   * `sortBy`/`sortOrder` params already established for this identical need.
   */
  readonly sortBy?: string | undefined;
  readonly sortOrder?: string | undefined;
}
