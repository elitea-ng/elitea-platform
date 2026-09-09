// Icon substitutions (disclosed) — none of these five have a ported
// equivalent in `shared/ui/icons/` (verified: `ls src/shared/ui/icons/`).
// `@mui/icons-material`, single-default-import per file, matches
// `shared/ui/ControlsDropdown.tsx`'s own established substitution
// convention for its missing `DotsMenuIcon`. Sized via the `fontSize="small"`
// PROP, not `sx.fontSize` — R-T11 (`elitea/ad-hoc-font-size`) bans ad-hoc
// `sx` font sizes outright and these MUI icon components have no
// `shared/brand` typography-variant integration to size through instead;
// `fontSize="small"` (20px) is the closest of MUI's 3 named sizes to the
// baseline's `1rem` (16px) — same substitution `ui/folders/FolderItem.tsx`'s
// own `<DeleteOutlineIcon fontSize="small" />` already established for an
// identical baseline `sx={{fontSize:'1rem'}}` icon.
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import DriveFileMoveOutlinedIcon from '@mui/icons-material/DriveFileMoveOutlined';
import FileDownloadOutlinedIcon from '@mui/icons-material/FileDownloadOutlined';
import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { isPublicProject } from '@/entities/project';
import { t } from '@/shared/i18n';
import type { ControlsDropdownItem, ControlsDropdownLeafItem } from '@/shared/ui/ControlsDropdown';
import { CopyLinkIcon } from '@/shared/ui/icons/copy-link-icon';
import { EditIcon } from '@/shared/ui/icons/edit-icon';
import { OpenEyeIcon } from '@/shared/ui/icons/open-eye-icon';
import { PinIcon } from '@/shared/ui/icons/pin-icon';
import { PlayIcon } from '@/shared/ui/icons/play-icon';

import { menuIconStyle } from './ConversationItem.styles';
import type { ConversationWithOwnerMeta } from './ConversationItem.types';

/** `getConversationType` — `ConversationItem.jsx:101-106`. */
export function getConversationType(conversation: ConversationWithOwnerMeta): 'public' | 'private_with_users' | 'private_without_users' {
  if (!conversation.isPrivate) return 'public';
  return (conversation.usersCount ?? 1) > 1 ? 'private_with_users' : 'private_without_users';
}

/** `mainBodyWidth` — `ConversationItem.jsx:108-127`. */
export function computeMainBodyWidth(params: { readonly isHovering: boolean; readonly isPinned: boolean; readonly isPlayback: boolean; readonly conversationType: ReturnType<typeof getConversationType> }): string {
  const { isHovering, isPinned, isPlayback, conversationType } = params;
  let rightMargin = 0;
  if (isHovering) rightMargin += 32;
  if (isPinned && !isPlayback) rightMargin += 20;
  if (conversationType === 'private_with_users' || conversationType === 'public') rightMargin += 20;
  if (isPlayback) rightMargin += 24;
  return `calc(100% - ${rightMargin}px)`;
}

/** `chat_history[0]?.content` (`ConversationItem.jsx:395`) — narrowed without an unsafe cast, since `Conversation.chatHistory` is `readonly unknown[]`. */
export function firstMessagePreview(chatHistory: readonly unknown[] | undefined): string {
  const first = chatHistory?.[0];
  if (first === null || typeof first !== 'object' || !('content' in first)) return '';
  const content = (first as { content?: unknown }).content;
  return typeof content === 'string' ? content : '';
}

/** `projectId == PUBLIC_PROJECT_ID || projectId == personal_project_id` (`ConversationItem.jsx:238`) — the "Make public" item's visibility guard. Both sides default to "not restricted" when the corresponding id is unknown, rather than treating two unset values as a false match. */
export function isPublicOrPersonalProject(projectId: string | undefined, publicProjectId: string | number | undefined, personalProjectId: string | number | undefined): boolean {
  if (projectId === undefined) return false;
  if (publicProjectId !== undefined && isPublicProject(projectId, publicProjectId)) return true;
  return personalProjectId !== undefined && String(projectId) === String(personalProjectId);
}

/** `projectId == personal_project_id` (`ConversationItem.jsx:249`) — the "Share" item's visibility guard. */
export function isPersonalProject(projectId: string | undefined, personalProjectId: string | number | undefined): boolean {
  return projectId !== undefined && personalProjectId !== undefined && String(projectId) === String(personalProjectId);
}

export interface MenuItemsParams {
  readonly conversation: ConversationWithOwnerMeta;
  readonly isActive: boolean;
  readonly isEditingCanvas: boolean;
  readonly currentUserId: string | undefined;
  readonly moveToFoldersMenuItems: readonly ControlsDropdownLeafItem[];
  readonly canMoveToFolders: boolean;
  readonly isPublicOrPersonal: boolean;
  readonly isPersonal: boolean;
  readonly theme: Theme;
  readonly onDelete: () => void;
  readonly onEdit: () => void;
  /**
   * Downloads the conversation in the format the menu entry names (issue
   * 851). Still optional, and still the thing that ENABLES the entry: a
   * caller that supplies no exporter gets a disabled "Export" row rather
   * than one that opens onto options wired to nothing.
   */
  readonly onExport?: ((format: ConversationExportFormat) => void) | undefined;
  readonly onMakePublic: () => void;
  readonly onShare: () => void;
  /**
   * Opens the share-by-link dialog. Distinct from `onShare`, which copies an
   * INTERNAL `/chat/{id}?shared_chat=1` URL that still requires a session and
   * project membership to open. The two are different products: one points a
   * colleague at the app, the other publishes the transcript to anyone holding
   * a URL. Collapsing them into one menu entry is how a user reaches for the
   * first and gets the second.
   */
  readonly onShareByLink: () => void;
  readonly onPlayback: () => void;
  readonly onPin: () => void;
}

/** Playback rows only ever offer Delete/Edit (`ConversationItem.jsx:264-278`). */
export function buildPlaybackMenuItems(params: Pick<MenuItemsParams, 'onDelete' | 'onEdit'>): ControlsDropdownItem[] {
  return [
    {
      key: 'delete',
      label: t('features.chatConversationList.conversationItem.menu.delete', 'Delete'),
      icon: <DeleteOutlineIcon fontSize="small" />,
      confirm: {
        message: t('features.chatConversationList.conversationItem.menu.deletePlaybackConfirm', 'Are you sure to delete playback?'),
        confirmLabel: t('features.chatConversationList.conversationItem.menu.delete', 'Delete'),
        onConfirm: params.onDelete,
      },
    },
    {
      key: 'edit',
      label: t('features.chatConversationList.conversationItem.menu.edit', 'Edit'),
      icon: <EditIcon style={menuIconStyle} />,
      onClick: params.onEdit,
    },
  ];
}

function buildDeleteEditItems(params: MenuItemsParams, deleteEditDisabled: boolean, secondaryFillSx: SxProps<Theme>): ControlsDropdownItem[] {
  return [
    {
      key: 'delete',
      label: t('features.chatConversationList.conversationItem.menu.delete', 'Delete'),
      icon: <DeleteOutlineIcon fontSize="small" />,
      disabled: deleteEditDisabled,
      confirm: {
        message: t('features.chatConversationList.conversationItem.menu.deleteConfirm', "Are you sure to delete conversation? It can't be restored."),
        confirmLabel: t('features.chatConversationList.conversationItem.menu.delete', 'Delete'),
        onConfirm: params.onDelete,
      },
    },
    {
      key: 'edit',
      label: t('features.chatConversationList.conversationItem.menu.edit', 'Edit'),
      icon: (
        <Box sx={secondaryFillSx}>
          <EditIcon style={menuIconStyle} />
        </Box>
      ),
      disabled: deleteEditDisabled,
      onClick: params.onEdit,
    },
  ];
}

/**
 * The formats the export route serves.
 *
 * Declared here rather than imported from `entities/conversation`, which owns
 * the fetcher: that slice's public API is exactly at its export budget, and
 * `no-deep-slice-import-cross-slice` forbids this feature reaching past its
 * `index.ts` for one string union. The two definitions are structurally
 * identical, so the exporter this feeds still type-checks against the entity's
 * own parameter — a value outside the union cannot reach it.
 */
export type ConversationExportFormat = 'md' | 'json';

/**
 * The two formats the export route serves, as the two menu entries that were
 * placeholders until issue 851.
 *
 * The labels name a FORMAT and its file extension, because that is the only
 * thing that distinguishes the two choices for the person clicking: one saves
 * a document to read, the other saves data to keep.
 *
 * `ControlsDropdownLeafItem.onClick` is optional but NOT `| undefined`-widened
 * (external type), so the key is spread in conditionally rather than assigned
 * a possibly-`undefined` value — required by this codebase's
 * `exactOptionalPropertyTypes: true`.
 */
function buildExportFormatItems(onExport: ((format: ConversationExportFormat) => void) | undefined): ControlsDropdownLeafItem[] {
  const formats: readonly { readonly key: string; readonly format: ConversationExportFormat; readonly label: string }[] = [
    { key: 'export-markdown', format: 'md', label: t('features.chatConversationList.conversationItem.menu.exportMarkdown', 'Markdown (.md)') },
    { key: 'export-json', format: 'json', label: t('features.chatConversationList.conversationItem.menu.exportJson', 'JSON (.json)') },
  ];
  return formats.map(({ key, format, label }) => ({
    key,
    label,
    ...(onExport !== undefined ? { onClick: () => onExport(format) } : {}),
  }));
}

function buildMoveAndExportItems(params: MenuItemsParams, isEditingActive: boolean): ControlsDropdownItem[] {
  return [
    {
      key: 'move-to',
      label: t('features.chatConversationList.conversationItem.menu.moveTo', 'Move to'),
      icon: <DriveFileMoveOutlinedIcon fontSize="small" />,
      disabled: params.conversation.isPinned === true || !params.canMoveToFolders || isEditingActive,
      items: [...params.moveToFoldersMenuItems],
    },
    {
      key: 'export',
      label: t('features.chatConversationList.conversationItem.menu.export', 'Export'),
      icon: <FileDownloadOutlinedIcon fontSize="small" />,
      // ISSUE 851. This used to be `disabled: true` with two children
      // labelled `Option1`/`Option2` that wired an `onClick` only if an
      // `onExport` happened to be supplied — and no caller supplied one, so
      // the entry was a named control that did nothing in every build. It is
      // now driven by whether an exporter really is wired: the row is live
      // exactly when there is something behind it, which is the only state a
      // reader can act on.
      disabled: params.onExport === undefined,
      items: buildExportFormatItems(params.onExport),
    },
  ];
}

/**
 * Returns true when the current user is not the author of this conversation.
 * The row menu then keeps Delete and Edit disabled.
 *
 * The former test was `String(currentUserId) !== String(conversation.authorId)`.
 * Both operands were always `undefined` in the running app. The folders wire
 * normaliser dropped `author_id`. The composition root never passed
 * `currentUserId`. The test was therefore always false, and any project member
 * could delete another member's conversation.
 *
 * The test now fails closed. An unknown identity on either side denies the
 * action.
 *
 * The `isNew` branch is the one exemption. No production code sets `isNew` on
 * a sidebar row today. `isDraftConversation` in `Conversations.helpers.ts`
 * reads the same absent field. Keep the branch, because a local draft has no
 * author id and belongs to the current user.
 */
function isNotAuthorOf(conversation: ConversationWithOwnerMeta, currentUserId: string | undefined): boolean {
  if (conversation.isNew === true) return false;
  if (currentUserId === undefined || conversation.authorId === undefined) return true;
  return String(currentUserId) !== String(conversation.authorId);
}

/**
 * The full, non-playback menu (`ConversationItem.jsx:166-263`). `alertTitle`/
 * `alarm` (baseline Delete item) have no `ControlsDropdownConfirmConfig`
 * equivalent — `shared/ui/ControlsDropdown.tsx`'s own doc comment already
 * discloses that scope cut ("drops the modal confirmation route... drops
 * multi-column layout"), not re-derived here; `confirm.message` carries the
 * confirmation copy alone. The "Move to" item's baseline `ArrowRightIcon`
 * trailing indicator is also dropped: `ControlsDropdownItem` renders no
 * nested-flyout affordance of its own (`aria-haspopup` only), so there is
 * nowhere to put it without changing that shared component.
 */
export function buildActiveMenuItems(params: MenuItemsParams): ControlsDropdownItem[] {
  const { conversation, isActive, isEditingCanvas, currentUserId, isPublicOrPersonal, isPersonal, theme } = params;
  const isEditingActive = isActive && isEditingCanvas;
  const deleteEditDisabled = isNotAuthorOf(conversation, currentUserId) || isEditingActive;
  const secondaryFillSx: SxProps<Theme> = { svg: { path: { fill: theme.vars.palette.secondary.main } } };

  const items: ControlsDropdownItem[] = [...buildDeleteEditItems(params, deleteEditDisabled, secondaryFillSx), ...buildMoveAndExportItems(params, isEditingActive)];

  if (conversation.isPrivate && !isPublicOrPersonal) {
    items.push({
      key: 'make-public',
      label: t('features.chatConversationList.conversationItem.menu.makePublic', 'Make public'),
      icon: <OpenEyeIcon style={menuIconStyle} />,
      disabled: isEditingActive,
      confirm: {
        message: t('features.chatConversationList.conversationItem.menu.makePublicConfirm', 'Are you sure to make your conversation public?'),
        confirmLabel: t('features.chatConversationList.conversationItem.menu.makePublic', 'Make public'),
        onConfirm: params.onMakePublic,
      },
    });
  }

  if (!isPersonal) {
    items.push({
      key: 'share',
      label: t('features.chatConversationList.conversationItem.menu.share', 'Share'),
      icon: (
        <Box sx={secondaryFillSx}>
          <CopyLinkIcon style={menuIconStyle} />
        </Box>
      ),
      onClick: params.onShare,
    });
  }

  items.push({
    key: 'share-by-link',
    label: t('features.chatConversationList.conversationItem.menu.shareByLink', 'Share by link'),
    icon: (
      <Box sx={secondaryFillSx}>
        <CopyLinkIcon style={menuIconStyle} />
      </Box>
    ),
    disabled: isEditingActive,
    onClick: params.onShareByLink,
  });

  items.push({
    key: 'playback',
    label: t('features.chatConversationList.conversationItem.menu.playback', 'Playback'),
    icon: <PlayIcon style={menuIconStyle} />,
    disabled: isEditingActive,
    onClick: params.onPlayback,
  });

  items.push({
    key: 'pin',
    label: conversation.isPinned === true ? t('features.chatConversationList.conversationItem.menu.unpin', 'Unpin') : t('features.chatConversationList.conversationItem.menu.pin', 'Pin on top'),
    icon: <PinIcon style={menuIconStyle} />,
    disabled: conversation.isPinned !== true && conversation.folderId !== undefined,
    onClick: params.onPin,
  });

  return items;
}
