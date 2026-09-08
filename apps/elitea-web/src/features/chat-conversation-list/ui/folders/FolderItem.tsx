import type { ChangeEvent, KeyboardEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import type { Theme } from '@mui/material/styles';

import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { ConversationNameRegExp } from '@/shared/lib/validation';
import { MAX_CONVERSATION_LENGTH } from '@/shared/lib/limits';
import { PERMISSIONS } from '@/shared/lib/permissions';
import type { ControlsDropdownItem } from '@/shared/ui/ControlsDropdown';
import { EditIcon } from '@/shared/ui/icons/edit-icon';
import { PinIcon } from '@/shared/ui/icons/pin-icon';

import type { FolderListItem, NewFolderDraft } from '../../lib/hooks/conversationListState.types';
import { useHasPermission } from '../../lib/useHasPermission';
import type { UseCreateFolderResult } from '../../lib/hooks/useCreateFolder.hooks';
import type { UseDeleteFolderResult } from '../../lib/hooks/useDeleteFolder.hooks';
import type { UseEditFolderResult } from '../../lib/hooks/useEditFolder';
import type { DropAreaState } from '../../lib/hooks/useDragAndDrop';
import type { UseMoveToFolderConversationResult } from '../../lib/hooks/useMoveToFolderConversation.hooks';
import type { RenderConversationItem } from '../groups/DateGroup';
import { DraggableFolderItem } from './DraggableFolderItem';
import { DroppableFolderItem } from './DroppableFolderItem';
import { FolderAccordion } from './FolderAccordion';
import type { FolderAccordionSlotProps } from './FolderAccordion';
import { FolderAccordionItem } from './FolderAccordionItem';
import { FolderItemEditor } from './FolderItemEditor';

/** The folder-CRUD callbacks this component needs, derived from the 3 real hook result types (`useCreateFolder`/`useEditFolder`/`useDeleteFolder`) rather than re-declared by hand — keeps this bag's signatures from drifting out of sync with the hooks that actually implement them. */
export type FolderItemCallbacks = Pick<UseCreateFolderResult, 'onCreateFolder' | 'onCancelCreateFolder'> &
  Pick<UseEditFolderResult, 'onEditFolder' | 'onPinFolder'> &
  Pick<UseDeleteFolderResult, 'onDeleteFolder'> & {
    readonly onChangeActiveFolderName?: ((name: string) => void) | undefined;
  };

export type FolderMoveTargetCallbacks = Pick<UseMoveToFolderConversationResult, 'moveTargetConversationToNewFolder' | 'cancelMovingTargetConversationToNewFolder'>;

export interface FolderDragAndDropProps {
  readonly enableDragAndDrop?: boolean | undefined;
  readonly isDropDisabled?: boolean | undefined;
  readonly isDragDisabled?: boolean | undefined;
  readonly getDropAreaState?: ((dropAreaId: string) => DropAreaState) | undefined;
  readonly isNextFolderHovered?: boolean | undefined;
  readonly onFolderHover?: ((folderId: string, isHovered: boolean) => void) | undefined;
}

export interface FolderLoadMoreProps {
  readonly onLoadMoreInFolder?: ((folderId: string) => void) | undefined;
  readonly isLoadingMoreInFolder?: boolean | undefined;
}

export interface FolderItemProps {
  readonly folder: FolderListItem;
  /** N4 signature deviation (this codebase's established convention, see `entities/conversation/lib/hooks/useConversationLifecycle.ts`'s own doc comment): explicit, not resolved internally — needed for this component's own `useHasPermission` call. */
  readonly projectId: string | undefined;
  readonly isActive?: boolean | undefined;
  readonly containsActiveConversation?: boolean | undefined;
  readonly renderConversationItem: RenderConversationItem;
  readonly callbacks: FolderItemCallbacks;
  readonly moveTarget: FolderMoveTargetCallbacks;
  readonly dragAndDrop?: FolderDragAndDropProps | undefined;
  readonly loadMore?: FolderLoadMoreProps | undefined;
}

/** Baseline: `!folder.id` / falsy destructures off a plain JS object. `folder.targetConversationId` only exists on the `NewFolderDraft` extension of `FolderListItem` (drag-to-"new folder" flow) — read via a downcast rather than widening `FolderListItem` itself for every caller. */
function readTargetConversationId(folder: FolderListItem): string | undefined {
  return (folder as Partial<NewFolderDraft>).targetConversationId;
}

/**
 * `owner_id` — disclosed real gap. `entities/folder`'s `Folder` type does
 * not model an owner id at all: `foldersApi.ts`'s `normaliseFolder` builds
 * the domain `Folder` from exactly `{id, name, conversations, total,
 * offset, isPinned}` and drops every other wire key, even though the wire
 * type's own index signature (`FolderWire`'s `[key: string]: unknown`)
 * would preserve one if the backend sends it. The OLD app
 * (`FolderItem.jsx:561`, `const { name, isNew: isNewFolder, owner_id } =
 * folder;`) destructures `owner_id` directly and compares it against
 * `state.user.id` with a loose `!=` — strong evidence the field is real on
 * the wire, just not (yet) modelled by this codebase's normaliser. Widening
 * `entities/folder`'s `Folder`/this slice's `FolderListItem` is out of this
 * unit's file-boundary scope (`ui/folders/` only) — read here via a
 * permissive cast instead, and compared as strings (not `===` on whatever
 * raw type the wire sends), since the baseline's own loose `!=` is itself a
 * hint that `owner_id` and the current user's id could disagree in type
 * (string vs number) even when they denote the same user.
 */
function readFolderOwnerId(folder: FolderListItem): string | undefined {
  const ownerId = (folder as unknown as { readonly owner_id?: string | number }).owner_id;
  return ownerId === undefined ? undefined : String(ownerId);
}

/**
 * Current-user id, resolved via `useGetCurrentAuthor` (`shared/api/
 * generated/social/social.ts`) per this unit's brief — `entities/author`'s
 * `isCurrentUserAuthor` selector does not fit here: it compares two AUTHOR
 * ids, but a folder's `owner_id` (see `readFolderOwnerId` above) is never
 * wrapped in a real `Author` object anywhere in this slice, so building one
 * just to satisfy that selector's signature would be manufacturing data
 * that does not exist. A direct string comparison at the ownership-check
 * call site is simpler and equally correct.
 *
 * The cast mirrors `lib/useHasPermission.ts`'s own established precedent:
 * `getCurrentAuthor`'s declared response type is a `{data: SocialAuthorProfile}
 * | {data: N401Response}` union (both variants have a `.data`, just
 * differently shaped) because `eliteaFetch` THROWS on a non-2xx response
 * rather than resolving with the error variant (§3.6 unwrap contract) — the
 * 401 branch is declared but unreachable at this read site.
 */
function useCurrentUserId(): string | undefined {
  const query = useGetCurrentAuthor();
  return (query.data?.data as SocialAuthorProfile | undefined)?.id;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/ui/
 * folders/FolderItem.jsx` (unit C2/folders) — the big one: renders either
 * the read-only `FolderAccordion` (optionally wrapped in `DraggableFolderItem`
 * + `DroppableFolderItem`) or an inline rename/create editor
 * (`StyledInputEnhancer`), and owns the `ControlsDropdown` `menuItems` build
 * (Delete/Edit/Export-disabled/Pin-Unpin) gated by BOTH permission
 * (`useHasPermission`, `PERMISSIONS.chat.folders.*`) AND ownership
 * (`folder.owner_id === currentUserId`, both doc-commented above).
 *
 * `useCheckPermission()` (baseline global hook) -> `useHasPermission(projectId,
 * permission)` (this slice's own N4-deviated, duplicated-per-feature
 * equivalent — see `lib/useHasPermission.ts`'s own doc comment).
 * `useSelector(state => state.user)` (baseline Redux) -> `useGetCurrentAuthor()`
 * (this unit's own choice, doc-commented above at `useCurrentUserId`).
 *
 * `alertTitle`/`alarm` (baseline Delete row fields — a "danger" visual
 * treatment on the old `DotMenu`'s confirm dialog) have no equivalent on
 * `ControlsDropdown`'s `confirm` shape (`{message, confirmLabel,
 * cancelLabel, onConfirm}` only) — dropped, disclosed, same class of gap as
 * `FolderAccordion.tsx`'s own `onShowMenuList`/`onCloseMenuList` note.
 * The Export row's baseline `onClick: handleEditFolder` (alongside its own
 * `subMenuItems`) is NOT ported: `ControlsDropdown`'s `hasNestedItems`
 * render path never calls a row's own `onClick` once it has nested `items`
 * (`ControlsDropdown.tsx:181-197`), so that baseline field was already
 * unreachable dead weight, not real behaviour to preserve byte-faithfully.
 */
export function FolderItem({
  folder,
  projectId,
  isActive = false,
  containsActiveConversation = false,
  renderConversationItem,
  callbacks,
  moveTarget,
  dragAndDrop,
  loadMore,
}: FolderItemProps): ReactNode {
  const { name, isNew: isNewFolder } = folder;

  const hasDeletePermission = useHasPermission(projectId, PERMISSIONS.chat.folders.delete);
  const hasUpdatePermission = useHasPermission(projectId, PERMISSIONS.chat.folders.update);
  const currentUserId = useCurrentUserId();
  const ownerId = readFolderOwnerId(folder);
  const isOwner = ownerId !== undefined && currentUserId !== undefined && ownerId === currentUserId;

  const [folderName, setFolderName] = useState(name);
  const [isHovering, setIsHovering] = useState(false);
  const [isFolderEditing, setIsFolderEditing] = useState(isNewFolder === true);

  const isFolderNameValid = useMemo(() => ConversationNameRegExp.test(folderName), [folderName]);

  const handleDeleteFolder = useCallback((): void => {
    void callbacks.onDeleteFolder(folder);
  }, [folder, callbacks]);

  const handleEditFolder = useCallback((): void => {
    setIsFolderEditing(true);
  }, []);

  const handlePinFolder = useCallback((): void => {
    void callbacks.onPinFolder(folder, folder.isPinned !== true);
  }, [folder, callbacks]);

  const menuItems = useMemo<ControlsDropdownItem[]>(
    () => [
      {
        key: 'delete',
        label: t('features.chatConversationList.folderItem.delete', 'Delete'),
        icon: <DeleteOutlinedIcon fontSize="small" />,
        disabled: !isOwner || !hasDeletePermission,
        confirm: {
          message: t('features.chatConversationList.folderItem.deleteConfirm', 'Are you sure to delete folder? It can’t be restored.'),
          confirmLabel: t('features.chatConversationList.folderItem.deleteConfirmLabel', 'Delete'),
          onConfirm: handleDeleteFolder,
        },
      },
      {
        key: 'edit',
        label: t('features.chatConversationList.folderItem.edit', 'Edit'),
        icon: <EditIcon style={{ width: '1rem', height: '1rem' }} />,
        disabled: !isOwner || !hasUpdatePermission,
        onClick: handleEditFolder,
      },
      {
        key: 'pin',
        label: folder.isPinned === true ? t('features.chatConversationList.folderItem.unpin', 'Unpin') : t('features.chatConversationList.folderItem.pinOnTop', 'Pin on top'),
        icon: <PinIcon style={{ width: '1rem', height: '1rem' }} />,
        disabled: !isOwner || !hasUpdatePermission,
        onClick: handlePinFolder,
      },
    ],
    [isOwner, hasDeletePermission, hasUpdatePermission, handleDeleteFolder, handleEditFolder, handlePinFolder, folder.isPinned],
  );

  const onMouseEnter = useCallback((): void => {
    setIsHovering(true);
    dragAndDrop?.onFolderHover?.(folder.id, true);
  }, [folder.id, dragAndDrop]);

  const onMouseLeave = useCallback((): void => {
    setIsHovering(false);
    dragAndDrop?.onFolderHover?.(folder.id, false);
  }, [folder.id, dragAndDrop]);

  const onChangeFolderName = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>): void => {
      const newName = event.target.value.slice(0, MAX_CONVERSATION_LENGTH);
      setFolderName(newName);
      if (isNewFolder === true) callbacks.onChangeActiveFolderName?.(newName);
    },
    [isNewFolder, callbacks],
  );

  const handleOnSaveFolder = useCallback((): void => {
    void callbacks.onEditFolder({ ...folder, name: folderName });
    setIsFolderEditing(false);
  }, [folder, folderName, callbacks]);

  const handleOnCreateFolder = useCallback(async (): Promise<void> => {
    const targetConversationId = readTargetConversationId(folder);
    if (targetConversationId === undefined) {
      await callbacks.onCreateFolder({ ...folder, name: folderName });
    } else {
      await moveTarget.moveTargetConversationToNewFolder({ ...folder, name: folderName } as NewFolderDraft);
    }
    setIsFolderEditing(false);
  }, [folder, folderName, callbacks, moveTarget]);

  const handleOnCancelCreateFolder = useCallback((): void => {
    // Baseline asymmetry, preserved (`FolderItem.jsx:706-714`): `setIsFolderEditing(false)`
    // is only reached on the "not a move-to-new-folder" branch below — harmless either way,
    // since `onCancelCreateFolder` removes this folder from the parent's list on both
    // branches, unmounting this component regardless of local `isFolderEditing` state.
    const targetConversationId = readTargetConversationId(folder);
    if (targetConversationId === undefined) {
      callbacks.onCancelCreateFolder(folder);
      setIsFolderEditing(false);
    } else {
      moveTarget.cancelMovingTargetConversationToNewFolder(folder as NewFolderDraft);
      callbacks.onCancelCreateFolder(folder);
    }
  }, [folder, callbacks, moveTarget]);

  const handleOnCloseEditFolder = useCallback((): void => {
    setFolderName(name);
    setIsFolderEditing(false);
  }, [name]);

  const handleConfirmEdit = useCallback((): void => {
    if (isNewFolder === true) void handleOnCreateFolder();
    else handleOnSaveFolder();
  }, [isNewFolder, handleOnCreateFolder, handleOnSaveFolder]);

  const handleCancelEdit = useCallback((): void => {
    if (isNewFolder === true) handleOnCancelCreateFolder();
    else handleOnCloseEditFolder();
  }, [isNewFolder, handleOnCancelCreateFolder, handleOnCloseEditFolder]);

  const handleOnKeyDownFolder = useCallback(
    // `StyledInputEnhancer`'s `onKeyDown` resolves to MUI `TextField`'s own
    // `KeyboardEventHandler<HTMLDivElement>` (the root `FormControl` element
    // `TextField` types its DOM event handlers against, not the inner
    // `<input>`) — same resolution `ConversationItem.tsx`'s own `onKeyDown`
    // doc comment already established for the identical situation.
    (event: KeyboardEvent<HTMLDivElement>): void => {
      if (event.key === 'Enter' && isFolderNameValid) handleConfirmEdit();
    },
    [isFolderNameValid, handleConfirmEdit],
  );

  const slotProps: FolderAccordionSlotProps = useMemo(
    () => ({
      summary: { sx: (accordionTheme: Theme) => ({ padding: accordionTheme.spacing(0, 0.5) }) },
      summaryContainer: { sx: (accordionTheme: Theme) => ({ padding: accordionTheme.spacing(0.75, 0.5), height: '2.5625rem' }) },
      detail: { sx: (accordionTheme: Theme) => ({ paddingLeft: accordionTheme.spacing(2) }) },
      sx: (accordionTheme: Theme) => ({ background: accordionTheme.vars.palette.background.tabPanel }),
    }),
    [],
  );

  const folderAccordion = (
    <FolderAccordion
      menuItems={menuItems}
      isActive={isActive}
      interaction={{ isHovering, isNextFolderHovered: dragAndDrop?.isNextFolderHovered, onMouseEnter, onMouseLeave }}
      defaultExpanded={containsActiveConversation}
      isPinned={folder.isPinned}
      items={[
        {
          title: name,
          content: (
            <FolderAccordionItem
              folder={folder}
              renderConversationItem={renderConversationItem}
              onLoadMore={() => loadMore?.onLoadMoreInFolder?.(folder.id)}
              isLoadingMore={loadMore?.isLoadingMoreInFolder}
            />
          ),
        },
      ]}
      slotProps={slotProps}
    />
  );

  if (isFolderEditing) {
    return (
      <FolderItemEditor
        folderName={folderName}
        isFolderNameValid={isFolderNameValid}
        onChangeFolderName={onChangeFolderName}
        onKeyDown={handleOnKeyDownFolder}
        onConfirm={handleConfirmEdit}
        onCancel={handleCancelEdit}
      />
    );
  }

  if (dragAndDrop?.enableDragAndDrop === true) {
    const dropAreaState = dragAndDrop.getDropAreaState?.(`folder-${folder.id}`) ?? { isValidDropTarget: true, isActive: true };
    return (
      <DraggableFolderItem
        folder={folder}
        isDragDisabled={dragAndDrop.isDragDisabled}
      >
        <DroppableFolderItem
          folder={folder}
          isDropDisabled={dragAndDrop.isDropDisabled}
          isValidDropTarget={dropAreaState.isValidDropTarget}
          isActive={dropAreaState.isActive}
        >
          {folderAccordion}
        </DroppableFolderItem>
      </DraggableFolderItem>
    );
  }

  return folderAccordion;
}
