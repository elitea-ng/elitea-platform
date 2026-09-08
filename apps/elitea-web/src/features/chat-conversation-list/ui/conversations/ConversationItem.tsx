import type { ChangeEvent, ReactNode } from 'react';
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { useTheme } from '@mui/material/styles';

import { MAX_CONVERSATION_LENGTH } from '@/shared/lib/limits';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { SearchParams } from '@/shared/lib/params';
import { ConversationNameRegExp } from '@/shared/lib/validation';
import { handleCopy } from '@/shared/lib/clipboard';
import type { ControlsDropdownItem, ControlsDropdownLeafItem } from '@/shared/ui/ControlsDropdown';

import { useHasPermission } from '../../lib/useHasPermission';
import { conversationItemStyles } from './ConversationItem.styles';
import {
  buildActiveMenuItems,
  buildPlaybackMenuItems,
  computeMainBodyWidth,
  firstMessagePreview,
  getConversationType,
  isPersonalProject,
  isPublicOrPersonalProject,
} from './ConversationItem.menu';
import type { ConversationExportFormat } from './ConversationItem.menu';
import { ConversationItemEditor } from './ConversationItem.editor';
import { ConversationItemRow } from './ConversationItem.row';
import type { ConversationWithOwnerMeta } from './ConversationItem.types';
import { DraggableConversationItem } from './DraggableConversationItem';
import { ConversationShareDialog } from './ConversationItem.share';

export type { ConversationWithOwnerMeta } from './ConversationItem.types';

/**
 * Every optional field below is `?: T | undefined`, not just `?: T` — this
 * codebase's `tsconfig.json` sets `exactOptionalPropertyTypes: true`, under
 * which `field?: T` means the KEY may be absent but, if present, may not be
 * `undefined`. `Conversations.tsx`'s `renderConversationItem` passes several
 * of these straight through from its OWN optional props via plain JSX
 * attributes (`projectId={projectId}`, etc.) — an always-present attribute
 * whose value happens to be `undefined` — so the explicit `| undefined` is
 * required, not decorative. Same convention already established throughout
 * `shared/ui` (e.g. `StateVariableIconButtonProps`, `FolderAccordionProps`).
 */
export interface ConversationItemProps {
  readonly conversation: ConversationWithOwnerMeta;
  readonly onSelectConversation: (conversation: ConversationWithOwnerMeta) => void;
  readonly isActive?: boolean | undefined;
  readonly onDelete: (conversation: ConversationWithOwnerMeta) => void;
  /**
   * Downloads this conversation's transcript in the named format (issue 851).
   * Still optional, and still what decides whether the Export row is live:
   * the row is disabled when no exporter is supplied, so a surface that does
   * not offer export shows a disabled entry rather than live options wired to
   * nothing.
   */
  readonly onExport?: ((format: ConversationExportFormat) => void) | undefined;
  readonly onEdit: (conversation: ConversationWithOwnerMeta) => void;
  readonly onPlayback: (conversation: ConversationWithOwnerMeta) => void;
  readonly onPin: (conversation: ConversationWithOwnerMeta, shouldPin: boolean) => void;
  readonly onCreateConversation: (conversation: ConversationWithOwnerMeta) => Promise<unknown>;
  readonly onCancelCreate: () => void;
  readonly onChangeActiveConversationName: (name: string) => void;
  readonly moveToFoldersMenuItems?: readonly ControlsDropdownLeafItem[] | undefined;
  /**
   * Plain boolean PROP (baseline: `ConversationItem.jsx:50` already has it
   * as a prop too, not a `useSelector` read) — the caller (eventually a
   * page/composition-root unit) supplies it from `shared/lib/editorState.ts`'s
   * `useEditorStateStore`; this leaf component never reads that store itself.
   */
  readonly isEditingCanvas?: boolean | undefined;
  readonly enableDragAndDrop?: boolean | undefined;
  readonly isDragDisabled?: boolean | undefined;
  readonly isNextItemHovered?: boolean | undefined;
  readonly onItemHover?: ((itemId: string, isHovered: boolean) => void) | undefined;
  /**
   * N4 signature deviation (explicit param instead of an internal
   * `useSelectedProjectId()`/Redux read) — also doubles as the param
   * `useHasPermission` needs for the "Move to" item's permission check and
   * the param `handleShareConversation` needs to build the share link.
   */
  readonly projectId?: string | undefined;
  /**
   * Baseline: `const { id: userId, personal_project_id } = useSelector(state
   * => state.user);` (`ConversationItem.jsx:74`) — turned into three
   * explicit props (this one, `personalProjectId`, `publicProjectId` below),
   * same substitution instruction as `isEditingCanvas` above.
   * `currentUserId` is ALSO what `Conversations.jsx:82`'s own `state.user`
   * read resolves to (`userId`), so `Conversations.tsx` threads the exact
   * same prop value into both its own `getMoveConversationToFoldersMenuItems`
   * ownership check and this component.
   */
  readonly currentUserId?: string | undefined;
  readonly personalProjectId?: string | number | undefined;
  /**
   * Baseline: the module-level `PUBLIC_PROJECT_ID` constant
   * (`common/constants.js:14,61`, `+VITE_PUBLIC_PROJECT_ID`) — per
   * `entities/project/model/selectors.ts`'s own `isPublicProject` doc
   * comment, this is a per-deployment runtime-config value, not an
   * invented in-package constant, so it is a required parameter here too
   * (reused via `isPublicProject`, not re-derived).
   */
  readonly publicProjectId?: string | number | undefined;
  /**
   * DEPENDENCY-INJECTION DEVIATION (deliberate, documented) — same class
   * `shared/ui/CopyToClipboardButton.tsx`'s own doc comment already
   * establishes: "no shared toast infrastructure yet" (grepped, confirmed
   * again for this unit). The baseline calls `useToast().toastInfo(...)`
   * directly after copying the share link; this takes an `onShareLinkCopied`
   * callback instead so the caller decides how to surface it.
   */
  readonly onShareLinkCopied?: (() => void) | undefined;
  /**
   * Baseline: `getBasename()` from `@/routes` (`ConversationItem.jsx:34,155`).
   * `app/providers/basename.ts`'s `getAppBasename()` is the new equivalent,
   * but `features/` may not import from `app/` (R-L1, strict downward
   * layering) — an explicit prop instead, matching this file's own N4
   * convention for `projectId`. Defaults to `''` (root-relative), the same
   * fallback `getAppBasename()` itself resolves to outside a real router.
   */
  readonly basename?: string | undefined;
}

interface ResolvedConversationItemDefaults {
  readonly isActive: boolean;
  readonly moveToFoldersMenuItems: readonly ControlsDropdownLeafItem[];
  readonly isEditingCanvas: boolean;
  readonly enableDragAndDrop: boolean;
  readonly isDragDisabled: boolean;
  readonly isNextItemHovered: boolean;
  readonly basename: string;
  readonly isPlayback: boolean;
  readonly isPinned: boolean;
  readonly isNamingPending: boolean;
  readonly isNew: boolean;
}

/**
 * Collapses every `props.x ?? default` read into one call, the same
 * complexity-budget technique `shared/ui/InputBase.tsx`'s own
 * `resolveActions` doc comment explains ("each optional-chain/nullish-
 * coalescing operator is its own branch for `complexity`... folding five of
 * them straight into the component's body was what pushed it over budget").
 * Eleven optional fields (7 own props + 4 off `conversation`) would each add
 * one branch directly to `ConversationItem`'s own complexity if resolved
 * inline; extracted to its own function, they cost nothing there instead.
 */
function resolveConversationItemDefaults(props: ConversationItemProps): ResolvedConversationItemDefaults {
  return {
    isActive: props.isActive ?? false,
    moveToFoldersMenuItems: props.moveToFoldersMenuItems ?? [],
    isEditingCanvas: props.isEditingCanvas ?? false,
    enableDragAndDrop: props.enableDragAndDrop ?? false,
    isDragDisabled: props.isDragDisabled ?? false,
    isNextItemHovered: props.isNextItemHovered ?? false,
    basename: props.basename ?? '',
    isPlayback: props.conversation.isPlayback ?? false,
    isPinned: props.conversation.isPinned ?? false,
    isNamingPending: props.conversation.isNamingPending ?? false,
    isNew: props.conversation.isNew ?? false,
  };
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/ui/
 * conversations/ConversationItem.jsx` (unit C2). Split across
 * `ConversationItem.styles.ts` (sx builders), `ConversationItem.menu.tsx`
 * (`ControlsDropdown` items + small predicates), `ConversationItem.row.tsx`
 * (the non-editing row's presentation) and `ConversationItem.types.ts`
 * (the shared `ConversationWithOwnerMeta` widening) — purely to keep every
 * file under the §3.5 `max-lines`(400)/`complexity`(12) budgets; this file
 * owns state, the imperative handlers, and the two top-level render
 * branches (row vs. rename-editor). Imports `DraggableConversationItem`
 * directly from this same directory rather than through a barrel — this
 * slice's `ui/` has no `index.ts` (matches `features/pipelines/ui/`'s own
 * no-barrel, direct-relative-import precedent for a multi-directory `ui/`
 * feature).
 *
 * Two small, deliberate fixes over the baseline, both disclosed:
 *  - The baseline gives every row's menu-trigger wrapper the SAME literal
 *    `id="Menu"` (`ConversationItem.jsx:421`) — invalid HTML once more than
 *    one row is mounted (duplicate ids). `ControlsDropdown`'s own `id` prop
 *    is passed a per-row value (`conversation-menu-${conversation.id}`,
 *    `ConversationItem.row.tsx`) instead.
 *  - The baseline's `menuWrapper` visibility is driven by BOTH a CSS
 *    `&:hover #Menu { visibility: visible }` rule AND a JS `isHovering`
 *    state flag reaching the same conclusion (`ConversationItem.jsx:546-577`)
 *    — redundant even in the baseline. The CSS half existed only to outlast
 *    a mouse-leave while the menu stayed open (baseline `showMenu`, driven
 *    by `DotMenu`'s `onShowMenuList`/`onCloseMenuList` callbacks) —
 *    `ControlsDropdown` exposes no such open/close callback (its own doc
 *    comment: it "owns its own `Menu`/`MenuItem`/`MenuList` composition"
 *    with no external hook into that state), so there is nothing left for a
 *    CSS rule to add over the `isHovering`-driven `display` toggle alone. Not
 *    reproduced, rather than carried over as dead weight.
 */
export const ConversationItem = memo(function ConversationItem(props: ConversationItemProps): ReactNode {
  const { conversation, onSelectConversation, onDelete, onExport, onEdit, onPlayback, onPin, onCreateConversation, onCancelCreate, onChangeActiveConversationName, onItemHover, projectId, currentUserId, personalProjectId, publicProjectId, onShareLinkCopied } = props;
  const { name, chatHistory } = conversation;
  const { isActive, moveToFoldersMenuItems, isEditingCanvas, enableDragAndDrop, isDragDisabled, isNextItemHovered, basename, isPlayback, isPinned, isNamingPending, isNew } = resolveConversationItemDefaults(props);

  const theme = useTheme();
  const inputRef = useRef<HTMLInputElement | null>(null);

  const hasFolderCreatePermission = useHasPermission(projectId, PERMISSIONS.chat.folders.create);
  const hasFolderUpdatePermission = useHasPermission(projectId, PERMISSIONS.chat.folders.update);

  const [isHovering, setIsHovering] = useState(false);
  const [isShareDialogOpen, setIsShareDialogOpen] = useState(false);
  const [isEditing, setIsEditing] = useState(isNew && !isNamingPending);
  const [conversationName, setConversationName] = useState(name);

  const isConversationNameValid = useMemo(() => ConversationNameRegExp.test(conversationName ?? ''), [conversationName]);

  const conversationType = useMemo(() => getConversationType(conversation), [conversation]);
  const mainBodyWidth = useMemo(() => computeMainBodyWidth({ isHovering, isPinned, isPlayback, conversationType }), [isHovering, isPinned, isPlayback, conversationType]);
  const styles = useMemo(() => conversationItemStyles({ isActive, isHovering, isNextItemHovered, isConversationNameValid }), [isActive, isHovering, isNextItemHovered, isConversationNameValid]);

  // Sync local conversationName state with the name prop when it changes —
  // ensures the edit dialog shows the correct name after auto-calculation
  // (`ConversationItem.jsx:96-98`).
  useEffect(() => {
    setConversationName(name);
  }, [name]);

  // R-C1 (`jsx-a11y/no-autofocus`) bans the JSX `autoFocus` prop outright —
  // same fence `shared/ui/SimpleSearchBar.tsx`'s own doc comment already
  // hit for its own `autoFocus` baseline prop. This replaces it with the
  // same "caller with a real reason" escape hatch that doc comment invites:
  // imperative `.focus()` when entering edit mode, via `inputRef`.
  useEffect(() => {
    if (isEditing) inputRef.current?.focus();
  }, [isEditing]);

  const onClickConversation = useCallback(() => {
    if (!isActive) onSelectConversation(conversation);
  }, [conversation, isActive, onSelectConversation]);

  const handlePin = useCallback(() => {
    onPin(conversation, !isPinned);
  }, [conversation, isPinned, onPin]);

  const handleDelete = useCallback(() => {
    onDelete(conversation);
  }, [conversation, onDelete]);

  const handleEdit = useCallback(() => {
    setIsEditing(true);
  }, []);

  const handleMakePublic = useCallback(() => {
    if (conversation.isPrivate) onEdit({ ...conversation, isPrivate: false });
  }, [conversation, onEdit]);

  const handlePlayback = useCallback(() => {
    onPlayback(conversation);
  }, [conversation, onPlayback]);

  const handleShareConversation = useCallback(async () => {
    const baseUrl = `${window.location.protocol}//${window.location.host}`;
    const destinationUrl = `${baseUrl}${basename}/${String(projectId)}/chat/${conversation.id}?${SearchParams.Name}=${conversation.name.replaceAll(' ', '+')}&${SearchParams.SharedChat}=1`;
    await handleCopy(destinationUrl);
    onShareLinkCopied?.();
  }, [basename, projectId, conversation.id, conversation.name, onShareLinkCopied]);

  // `menuItems`'s own dependency list (below) breached the §3.5 `hook-deps`
  // budget (8) at 19 entries. Split into two smaller, still-exhaustive
  // memos — the row-context fields `buildActiveMenuItems` reads, and the
  // handler callbacks it wires up — purely to bring each `useMemo` call
  // under budget without weakening memoization (this recomputes on exactly
  // the same set of input changes as the single 19-entry list did; nothing
  // is read via a ref/skips the dependency list).
  const menuItemsContext = useMemo(
    () => ({
      conversation,
      isActive,
      isEditingCanvas,
      currentUserId,
      moveToFoldersMenuItems,
      canMoveToFolders: hasFolderCreatePermission && hasFolderUpdatePermission,
    }),
    [conversation, isActive, isEditingCanvas, currentUserId, moveToFoldersMenuItems, hasFolderCreatePermission, hasFolderUpdatePermission],
  );

  const menuItemsHandlers = useMemo(
    () => ({
      onDelete: handleDelete,
      onEdit: handleEdit,
      onExport,
      onMakePublic: handleMakePublic,
      onShare: () => void handleShareConversation(),
      onShareByLink: () => setIsShareDialogOpen(true),
      onPlayback: handlePlayback,
      onPin: handlePin,
    }),
    [handleDelete, handleEdit, onExport, handleMakePublic, handleShareConversation, handlePlayback, handlePin],
  );

  const menuItems = useMemo<ControlsDropdownItem[]>(() => {
    if (isPlayback) return buildPlaybackMenuItems({ onDelete: menuItemsHandlers.onDelete, onEdit: menuItemsHandlers.onEdit });

    return buildActiveMenuItems({
      ...menuItemsContext,
      isPublicOrPersonal: isPublicOrPersonalProject(projectId, publicProjectId, personalProjectId),
      isPersonal: isPersonalProject(projectId, personalProjectId),
      theme,
      ...menuItemsHandlers,
    });
  }, [isPlayback, menuItemsContext, menuItemsHandlers, projectId, publicProjectId, personalProjectId, theme]);

  const onMouseEnter = useCallback(() => {
    setIsHovering(true);
    onItemHover?.(conversation.id, true);
  }, [conversation.id, onItemHover]);

  const onMouseLeave = useCallback(() => {
    setIsHovering(false);
    onItemHover?.(conversation.id, false);
  }, [conversation.id, onItemHover]);

  const onChangeConversationName = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      const newName = event.target.value.slice(0, MAX_CONVERSATION_LENGTH);
      setConversationName(newName);
      if (isNew) onChangeActiveConversationName(newName);
    },
    [isNew, onChangeActiveConversationName],
  );

  const onSave = useCallback(() => {
    onEdit({ ...conversation, name: conversationName });
    setIsEditing(false);
  }, [conversation, conversationName, onEdit]);

  const onCreate = useCallback(async () => {
    await onCreateConversation({ ...conversation, name: conversationName });
    setIsEditing(false);
  }, [conversation, conversationName, onCreateConversation]);

  const confirmEdit = useCallback(() => {
    if (isNew) void onCreate();
    else onSave();
  }, [isNew, onCreate, onSave]);

  const onCancelEdit = useCallback(() => {
    if (isNew) onCancelCreate();
    else {
      setConversationName(name);
      setIsEditing(false);
    }
  }, [isNew, onCancelCreate, name]);

  const row = (
    <ConversationItemRow
      conversationId={conversation.id}
      name={name}
      firstMessagePreview={firstMessagePreview(chatHistory)}
      isPlayback={isPlayback}
      isPinned={isPinned}
      isNamingPending={isNamingPending}
      conversationType={conversationType}
      mainBodyWidth={mainBodyWidth}
      styles={styles}
      menuItems={menuItems}
      onClick={onClickConversation}
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
    />
  );

  // The share dialog is a SIBLING of the row, never inside it: the row is what
  // `DraggableConversationItem` wraps, and a dialog mounted inside a drag
  // source is torn down mid-interaction the moment a drag starts.
  const content = (
    <>
      {enableDragAndDrop ? (
        <DraggableConversationItem conversation={conversation} isActive={isActive} isDragDisabled={isDragDisabled || isEditingCanvas}>
          {row}
        </DraggableConversationItem>
      ) : (
        row
      )}
      <ConversationShareDialog open={isShareDialogOpen} projectId={projectId} conversationId={conversation.id} conversationName={name} onClose={() => setIsShareDialogOpen(false)} />
    </>
  );

  if (!isEditing) return content;

  return (
    <ConversationItemEditor
      inputRef={inputRef}
      conversationName={conversationName}
      isConversationNameValid={isConversationNameValid}
      styles={styles}
      onChangeConversationName={onChangeConversationName}
      onEnterKey={confirmEdit}
      onConfirm={confirmEdit}
      onCancel={onCancelEdit}
    />
  );
});
