/**
 * The state + hook wiring behind `ui/ChatConversationSidebar.tsx`.
 *
 * `features/chat-conversation-list` was built complete — `Conversations`, its
 * folder/date-group/drag-and-drop tree, and nine hooks — and then never
 * mounted (issue #128 residual: no testid on the live `/chat` page matched
 * `/folder/i`). Every one of those hooks takes its state containers as
 * parameters rather than owning them, precisely because the composition root
 * was meant to own them. This file is that owner.
 *
 * Split out of the component for the §3.5 `max-lines` budget; the component
 * next door is pure composition. Baseline: `apps/elitea-ui/src/pages/NewChat/
 * NewChat.jsx` owns exactly these containers (`folders`/`dateGroups`/
 * `pinnedConversations`/`conversations`/`activeFolder`) and passes exactly this
 * prop bundle to `<Conversations>` (`NewChat.jsx:1372-1412`).
 */
import { useCallback, useRef, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import type { Conversation } from '@/entities/conversation';
import { conversationApi } from '@/entities/conversation';
import { DEFAULT_FOLDER_NAME } from '@/entities/folder';
import type { ConversationsDateGroup, ConversationsProps, FolderListItem } from '@/features/chat-conversation-list';
import {
  useCreateFolder,
  useDeleteFolder,
  useEditFolder,
  useMoveToFolderConversation,
  usePinConversation,
  useQueryFoldersList,
  useReorderFolders,
} from '@/features/chat-conversation-list';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { useSelectedProject } from '@/widgets/app-shell';

import { chatBasename, draftFolderId } from './conversationSidebar.helpers';
import { useDuplicateConversation } from './useDuplicateConversation';

/** What the component needs beyond the `Conversations` prop bundle itself. */
export interface UseConversationSidebarResult {
  readonly conversationsProps: ConversationsProps;
  /** Transient error text for the local snackbar — this app has no global toast host. */
  readonly errorMessage: string | undefined;
  readonly onDismissError: () => void;
}

export function useConversationSidebar(): UseConversationSidebarResult {
  const navigate = useNavigate();
  const { project } = useSelectedProject();
  const projectId = project?.id;

  /*
   * The current user's id, for the row menu's author-only guard on Delete and
   * Edit. `Conversations` accepts `currentUserId` as an optional prop and this
   * composition root never set it, so the guard compared `undefined` with
   * `undefined` and denied nothing. Resolved the way the slice's own
   * `FolderItem` does (`useGetCurrentAuthor`), and reactively, so the menu
   * re-renders when the profile lands. The declared response type is a union
   * whose 401 branch is unreachable here — `eliteaFetch` throws on a non-2xx
   * answer — so the cast follows that established precedent.
   */
  const currentAuthor = useGetCurrentAuthor();
  const currentUserId = (currentAuthor.data?.data as SocialAuthorProfile | undefined)?.id;

  // ── The state containers every hook in the slice writes through ─────────
  const [folders, setFolders] = useState<readonly FolderListItem[]>([]);
  const [dateGroups, setDateGroups] = useState<readonly ConversationsDateGroup[]>([]);
  const [pinnedConversations, setPinnedConversations] = useState<readonly Conversation[]>([]);
  const [conversations, setConversations] = useState<readonly Conversation[]>([]);
  const [activeFolder, setActiveFolder] = useState<FolderListItem | undefined>(undefined);
  const [activeConversation, setActiveConversation] = useState<Conversation | undefined>(undefined);
  /**
   * Live mirrors of the two pieces of state a handler DEFERS on, read at CALL
   * time instead of out of the closure that captured them.
   *
   * A row's handlers reach this hook through `useRenderConversationItem`'s
   * `useCallback([])` render prop into a memoised `ConversationItem`, so a
   * handler reference outlives the render that built it. `deleteConversation`
   * asking its own closure "is this the open transcript?" answered `undefined`
   * and stranded the app on the just-deleted one; `renameConversation` ("which
   * conversation am I renaming?") and `onClickCreateNewFolder` ("is a draft
   * folder already open?") ask the same call-time question. Reading the ref
   * also keeps that state OUT of their dependency arrays, so their identity
   * stops churning on every selection — which is what let a memoised row hold
   * a pre-selection closure in the first place. The router-blocker staleness
   * lesson's "read live" fix.
   */
  const activeConversationRef = useRef(activeConversation);
  activeConversationRef.current = activeConversation;
  const activeFolderRef = useRef(activeFolder);
  activeFolderRef.current = activeFolder;
  const [collapsed, setCollapsed] = useState(false);
  const [searchQuery, setSearchQuery] = useState<string | undefined>(undefined);
  const [errorMessage, setErrorMessage] = useState<string | undefined>(undefined);

  const toastError = useCallback((message: string) => setErrorMessage(message), []);
  const onDismissError = useCallback(() => setErrorMessage(undefined), []);

  /**
   * The list's own restore-on-load path and a user CLICK are deliberately two
   * different callbacks. `useQueryFoldersList` invokes its
   * `onSelectConversation` for whatever the server reports as
   * `selectedConversationId` as soon as the listing lands
   * (useQueryFoldersList.hooks.ts:180-191); wiring that to a navigation made
   * every visit to `/chat` redirect to `/chat/:lastConversation`, so "start a
   * new chat" was unreachable — the first message went to an EXISTING
   * conversation and never POSTed a new one (J8/J9/J11 all red). Restoring the
   * highlight is local state; only a click changes the route.
   */
  const restoreSelectedConversation = useCallback((conversation: Conversation) => {
    setActiveConversation(conversation);
  }, []);

  const onSelectConversation = useCallback(
    (conversation: Conversation) => {
      setActiveConversation(conversation);
      // `playback: '0'` is stated, not omitted: a plain click on a row while
      // a playback is open must LEAVE playback, and the flag's default is
      // stripped from the emitted URL by `__root`'s `stripSearchParams`, so
      // stating it costs nothing in the address bar.
      void navigate({ to: '/chat/$conversationId', params: { conversationId: conversation.id }, search: { playback: '0' } });
    },
    [navigate],
  );

  // ── The slice's own hooks, each given the containers above ──────────────
  const foldersList = useQueryFoldersList({
    projectId,
    toastError,
    setFolders,
    setDateGroups,
    setPinnedConversations,
    onSelectConversation: restoreSelectedConversation,
    ...(searchQuery !== undefined ? { searchQuery } : {}),
  });

  const { onCreateFolder, onCancelCreateFolder } = useCreateFolder({
    projectId,
    folders,
    setActiveFolder,
    setFolders,
    toastError,
  });

  const { onDeleteFolder } = useDeleteFolder({ projectId, setFolders, toastError });

  const { onEditFolder, onPinFolder } = useEditFolder({
    projectId,
    activeFolder,
    setActiveFolder,
    setFolders,
    toastError,
  });

  const moveToFolder = useMoveToFolderConversation({
    projectId,
    setFolders,
    setActiveFolder,
    setConversations,
    toastError,
    conversations,
    folders,
  });

  const { onPinConversation } = usePinConversation({
    projectId,
    activeConversation,
    setActiveConversation,
    setPinnedConversations,
    setDateGroups,
    setFolders,
    toastError,
  });

  const { onReorderFolders, isFolderUpdate } = useReorderFolders({
    projectId,
    folders,
    setFolders,
    toastError,
  });

  /**
   * The one required callback with no hook behind it anywhere in the slice —
   * `Conversations.header.tsx`'s "Create folder" button calls it directly
   * (via `Conversations.tsx:208-212`). Baseline: `NewChat.jsx:1228-1238`,
   * reproduced including its re-entrancy guard: while a draft is already open,
   * a second click is a no-op rather than stacking a second draft. The draft
   * carries `DEFAULT_FOLDER_NAME` ('New folder'), what the baseline seeds and
   * what `ConversationNameRegExp` accepts, so the editor opens with a valid,
   * confirmable name; `FolderItem.tsx:175` renders it in edit mode off `isNew`.
   */
  const onClickCreateNewFolder = useCallback(() => {
    // "Is a draft already open?" is answered when the button is CLICKED, not
    // when this callback was built (see `activeFolderRef`).
    if (activeFolderRef.current?.isNew === true) return;
    const draft: FolderListItem = { id: draftFolderId(), name: DEFAULT_FOLDER_NAME, conversations: [], isNew: true };
    setActiveFolder(draft);
    setFolders((prev) => [draft, ...prev]);
  }, []);

  const onCollapsed = useCallback(() => setCollapsed((prev) => !prev), []);

  /**
   * Applies one list transform to EVERY container that holds conversation rows.
   * Exactly one such traversal on purpose: the bug class this module fixes was a
   * mutation applied to SOME containers (rows render from `dateGroups`/`folders`,
   * not `conversations`), and two hand-written walks re-open that door — a fifth
   * container added to one and not the other is the same silent no-op again.
   */
  const transformEveryConversationList = useCallback(
    (transform: (items: readonly Conversation[]) => readonly Conversation[]) => {
      setConversations((prev) => [...transform(prev)]);
      setPinnedConversations((prev) => [...transform(prev)]);
      setDateGroups((prev) => prev.map((group) => ({ ...group, conversations: [...transform(group.conversations)] })));
      setFolders((prev) => prev.map((folder) => ({ ...folder, conversations: [...transform(folder.conversations)] })));
    },
    [],
  );

  const patchConversationEverywhere = useCallback(
    (updated: Conversation) => {
      transformEveryConversationList((items) =>
        items.map((item) => (item.id === updated.id ? { ...item, ...updated } : item)),
      );
      setActiveConversation((prev) => (prev !== undefined && prev.id === updated.id ? { ...prev, ...updated } : prev));
    },
    [transformEveryConversationList],
  );

  const deleteConversation = useCallback(
    (conversation: Conversation) => {
      if (projectId === undefined) return;
      void conversationApi
        .remove({ projectId, id: conversation.id })
        .then(() => {
          // The row being deleted renders out of `dateGroups`/`folders`
          // (`Conversations.body.tsx`) — filtering only `conversations` and
          // `pinnedConversations`, as this used to, left the row on screen.
          transformEveryConversationList((items) => items.filter((item) => item.id !== conversation.id));
          // Read the active conversation LIVE (see `activeConversationRef`).
          if (activeConversationRef.current?.id === conversation.id) {
            // Route still points at the deleted transcript; fall back to the
            // blank `/chat` (baseline `NewChat.jsx:1141-1157` picks the next).
            setActiveConversation(undefined);
            void navigate({ to: '/chat' });
          }
        })
        .catch(() => toastError('Failed to delete the conversation'));
    },
    [projectId, navigate, toastError, transformEveryConversationList],
  );

  const renameConversation = useCallback(
    (name: string) => {
      // Rename whatever is open WHEN THE USER TYPES, not whatever was open
      // when this callback was built (see `activeConversationRef`).
      const active = activeConversationRef.current;
      if (projectId === undefined || active === undefined) return;
      void conversationApi.edit({ projectId, id: active.id, name }).catch(() => toastError('Failed to rename the conversation'));
    },
    [projectId, toastError],
  );

  const createConversation = useCallback(
    async (conversation: Conversation): Promise<unknown> => {
      if (projectId === undefined) return undefined;
      try {
        // `is_private` is required by the wire contract; the baseline's own create
        // path (`NewChat.jsx`'s `onCreateConversation`) defaults it to private.
        return await conversationApi.create({ projectId, name: conversation.name, is_private: conversation.isPrivate ?? true });
      } catch {
        toastError('Failed to create the conversation');
        return undefined;
      }
    },
    [projectId, toastError],
  );

  /**
   * The sidebar's per-row Play button.
   *
   * The gap this closes: it used to be `onSelectConversation`, on the
   * grounds that `PlaybackChatBox` "is not mounted by any route either" —
   * so the Play button and a plain click did the same thing, silently. Both
   * halves are now real: `processes/chat/ui/ChatPlayback.tsx` mounts the
   * box, and `?playback=1` on the conversation's own URL is what selects it
   * (see that file for why the signal travels through the URL).
   *
   * Selection still happens, because the row must highlight either way.
   */
  const onPlaybackConversation = useCallback(
    (conversation: Conversation) => {
      setActiveConversation(conversation);
      void navigate({
        to: '/chat/$conversationId',
        params: { conversationId: conversation.id },
        search: { playback: '1' },
      });
    },
    [navigate],
  );

  // Issue 940/A6 — Chat "Duplicate" action. Its own hook
  // (`useDuplicateConversation.ts`) purely for the §3.5 400-line file
  // budget — see that file's own module doc for the full behaviour and the
  // ELITEA-2616..2624 case-by-case rationale.
  const duplicateConversation = useDuplicateConversation({ projectId, toastError, setActiveConversation });

  /**
   * `ConversationItem` calls `onEdit` with the ALREADY-updated conversation for
   * both the rename editor's save (`ConversationItem.tsx`'s `onSave`) and the
   * menu's "Make public" (`handleMakePublic`), so this must persist, not merely
   * select — it used to be `setActiveConversation(conversation)` only, which
   * made both gestures silent no-ops. Baseline: `useEditConversation.hooks.js`
   * — same PUT (name + is_private), same skip for a playback conversation, same
   * patch-on-success ordering (no optimistic write, so a failed PUT leaves the
   * list truthful).
   */
  const onEditConversation = useCallback(
    (conversation: Conversation) => {
      if (conversation.isPlayback === true) {
        // Baseline skips the network call for a playback conversation but
        // still applies the local patch (`useEditConversation.hooks.js:23`).
        patchConversationEverywhere(conversation);
        return;
      }
      if (projectId === undefined) return;
      void conversationApi
        .edit({ projectId, id: conversation.id, name: conversation.name, is_private: conversation.isPrivate })
        .then(() => patchConversationEverywhere(conversation))
        .catch(() => toastError('Failed to edit the conversation'));
    },
    [projectId, patchConversationEverywhere, toastError],
  );
  const onCancelCreateConversation = useCallback(() => setActiveConversation(undefined), []);

  /*
   * Deliberately NOT memoised. `Conversations` is a plain function component (no
   * `memo`), so a fresh props object costs nothing, and the baseline passes these
   * ~30 values as individual JSX props with no memo either. A `useMemo` here
   * would need all 27 in its dependency array — over the §3.5 `hook-deps` budget
   * of 8 — and would be wrong the moment one were forgotten.
   */
  const conversationsProps: ConversationsProps = {
    conversations,
    pinnedConversations,
    dateGroups,
    setDateGroups,
    ungroupedConversationsCount: conversations.length,
    totalConversationsAmount: dateGroups.reduce((sum, group) => sum + (group.total ?? group.conversations.length), 0),
    onSelectConversation,
    ...(activeConversation !== undefined ? { selectedConversationId: activeConversation.id } : {}),
    collapsed,
    onCollapsed,
    onEditConversation,
    onPlaybackConversation,
    onDeleteConversation: deleteConversation,
    // `usePinConversation` returns a Promise-valued handler; the prop is `(c,
    // shouldPin) => void`. Wrapped so the floating promise is explicitly discarded
    // (`typescript/no-misused-promises`); the hook toasts its own errors already.
    onPinConversation: (conversation, shouldPin) => void onPinConversation(conversation, shouldPin),
    onDuplicateConversation: (conversation) => void duplicateConversation(conversation),
    onCreateConversation: createConversation,
    onCancelCreateConversation,
    onChangeActiveConversationName: renameConversation,
    onCreateFolder,
    onCancelCreateFolder,
    folders,
    setFolders,
    onDeleteFolder,
    onEditFolder,
    onPinFolder,
    onMoveToFolderConversation: moveToFolder.onMoveToFolderConversation,
    onMoveToNewFolderConversation: moveToFolder.onMoveToNewFolderConversation,
    moveTargetConversationToNewFolder: moveToFolder.moveTargetConversationToNewFolder,
    cancelMovingTargetConversationToNewFolder: moveToFolder.cancelMovingTargetConversationToNewFolder,
    onClickCreateNewFolder,
    toastError,
    onReorderFolders,
    onSearchQueryChange: setSearchQuery,
    isLoadConversations: foldersList.isLoadFolders,
    isFolderOperationInProgress: isFolderUpdate || foldersList.isLoadFolders || foldersList.isLoadMoreFolders,
    basename: chatBasename(),
    ...(projectId !== undefined ? { projectId } : {}),
    ...(currentUserId !== undefined ? { currentUserId } : {}),
  };

  return { conversationsProps, errorMessage, onDismissError };
}
