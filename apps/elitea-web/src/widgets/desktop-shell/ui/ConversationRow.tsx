/**
 * One conversation in the sidebar's Chats list: a native row (`ShellRow`)
 * that opens it, with the same actions on a right-click (the OS menu,
 * `useContextMenu`) and on its hover "⋯" button: Pin / Unpin, Rename,
 * Move to folder…, Fork (a new chat with the whole history, client contract
 * 1.11), Archive, Copy Link, Delete. Rename edits the title in
 * place; Move to folder… lists the project's chat folders (a pinned chat is
 * not moved, as in the web list). Archive and
 * Delete ask first: the archive is the project's (it takes the chat out of
 * every member's list; any member can unarchive it). Rename and Delete are
 * the author's, as in the web list.
 *
 * A row the person may move (not pinned, with the folder right) is a drag
 * source (`@dnd-kit`, `useDraggable` with the conversation id, the web
 * list's id scheme); `ChatsSection` owns the drop. The open conversation's
 * row scrolls into view when it becomes the selection.
 */
import { useEffect, useRef, useState, type KeyboardEvent } from 'react';

import { useDraggable } from '@dnd-kit/core';

import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import InputBase from '@mui/material/InputBase';
import Menu from '@mui/material/Menu';
import type { Theme } from '@mui/material/styles';

import type { Conversation } from '@/entities/conversation';
import { t } from '@/shared/i18n';
import { MenuDivider, MenuRow, MenuRowText, pressToOpen } from '@/shared/ui/native-menu';
import { activityGlyph, activityTitleBold, useRowActivity } from '@/shared/lib/conversationActivity';
import { ConversationActivityGlyph } from '@/shared/ui/ConversationActivity';
import { BaseModal } from '@/shared/ui/BaseModal';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { conversationLink, copyText } from '../lib/conversationLink';
import { useContextMenu, type ContextAction } from '../model/useContextMenu';
import { MoveToFolderDialog, type FolderChoice } from './MoveToFolderDialog';
import { ShellRow } from './rows';

export interface ConversationRowActions {
  readonly open: (conversation: Conversation) => void;
  readonly pin: (conversation: Conversation, shouldPin: boolean) => void;
  readonly rename: (conversation: Conversation, name: string) => void;
  readonly archive: (conversation: Conversation) => void;
  /** Forks the whole history into a new chat of the caller, and opens it. */
  readonly fork: (conversation: Conversation) => void;
  readonly remove: (conversation: Conversation) => void;
  readonly canModify: (conversation: Conversation) => boolean;
  /** Holds the project's archive right (`PERMISSIONS.chat.archive`); without it the server answers 403. */
  readonly canArchive: boolean;
  /** The project's chat folders, for Move to folder…. */
  readonly folders: readonly FolderChoice[];
  /** The folder that holds the conversation now; `null` when none does. */
  readonly folderOf: (conversation: Conversation) => string | null;
  readonly moveToFolder: (conversation: Conversation, folderId: string | null) => void;
  /** Holds the folder right (`PERMISSIONS.chat.folders.update`): Move to folder… and the drag. */
  readonly canMoveToFolder: boolean;
}

export interface ConversationRowProps {
  readonly conversation: Conversation;
  readonly selected: boolean;
  readonly indent: number;
  readonly actions: ConversationRowActions;
}

/** The row's actions, once, for both menus. Leaves only (the ⋯ menu renders the same list). */
interface RowAsks {
  readonly rename: () => void;
  readonly move: () => void;
  readonly archive: () => void;
  readonly remove: () => void;
}

function conversationActions(conversation: Conversation, actions: ConversationRowActions, asks: RowAsks): ContextAction[] {
  const link = conversationLink(conversation.id);
  const pinned = conversation.isPinned === true;
  const mine = actions.canModify(conversation);
  return [
    { id: 'open', label: t('desktop.menu.open', 'Open'), run: () => actions.open(conversation) },
    { separator: true },
    { id: 'pin', label: pinned ? t('desktop.chats.unpin', 'Unpin') : t('desktop.chats.pin', 'Pin'), run: () => actions.pin(conversation, !pinned) },
    { id: 'rename', label: t('desktop.chats.rename', 'Rename'), enabled: mine, run: asks.rename },
    {
      id: 'move',
      label: t('desktop.chats.moveToFolder', 'Move to folder…'),
      enabled: actions.canMoveToFolder && !pinned && (actions.folders.length > 0 || actions.folderOf(conversation) !== null),
      run: asks.move,
    },
    { id: 'fork', label: t('desktop.chats.fork', 'Fork'), run: () => actions.fork(conversation) },
    { id: 'archive', label: t('desktop.chats.archive', 'Archive'), enabled: actions.canArchive, run: asks.archive },
    { separator: true },
    { id: 'link', label: t('workspace.copyLink', 'Copy Link'), enabled: link !== null, run: () => link !== null && copyText(link) },
    { separator: true },
    { id: 'delete', label: t('desktop.chats.delete', 'Delete…'), enabled: mine, run: asks.remove },
  ];
}

/** The row's hover "⋯": the same leaves as its native context menu (the Local work thread rows use it too). */
export function RowMenu({ items, label, testId = 'shell-conversation-more' }: { readonly items: readonly ContextAction[]; readonly label: string; readonly testId?: string }): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchor(null);
  return (
    <>
      <IconButton size="small" aria-label={label} aria-haspopup="menu" data-testid={testId} {...pressToOpen(setAnchor)}>
        <MoreHorizIcon fontSize="inherit" />
      </IconButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} slotProps={{ list: { dense: true }, paper: { sx: { minWidth: '12rem' } } }}>
        {items.map((item, index) =>
          'separator' in item ? (
            <MenuDivider key={`separator-${String(index)}`} />
          ) : 'submenu' in item ? null : (
            <MenuRow
              key={item.id}
              disabled={item.enabled === false}
              onClick={() => {
                close();
                item.run();
              }}
            >
              <MenuRowText>{item.label}</MenuRowText>
            </MenuRow>
          ),
        )}
      </Menu>
    </>
  );
}

/** The title being edited in place: Enter saves, Escape or leaving the field cancels. */
function RenameField({ conversation, indent, onDone }: { readonly conversation: Conversation; readonly indent: number; readonly onDone: (name: string | null) => void }): React.JSX.Element {
  const [value, setValue] = useState(conversation.name);
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter') onDone(value.trim());
    else if (event.key === 'Escape') onDone(null);
  };
  return (
    <Box sx={{ paddingLeft: 1 + indent, paddingRight: 0.5, minHeight: '1.75rem', display: 'flex', alignItems: 'center' }}>
      <InputBase
        value={value}
        onChange={(event) => setValue(event.target.value)}
        onKeyDown={onKeyDown}
        onBlur={() => onDone(null)}
        inputRef={(node: HTMLInputElement | null) => node?.focus()}
        inputProps={{ 'aria-label': t('desktop.chats.renameLabel', 'Conversation name'), 'data-testid': 'shell-conversation-rename' }}
        sx={(theme: Theme) => ({
          flex: 1,
          paddingX: 0.5,
          borderRadius: theme.vars.shape.radiusSm,
          border: `1px solid ${theme.vars.palette.primary.main}`,
          background: theme.vars.palette.background.default,
          typography: 'labelSmall',
        })}
      />
    </Box>
  );
}

/** The row as a drag source, while it may move; the wrapper takes pointer events only (the keyboard way is Move to folder…). */
function useRowDrag(conversation: Conversation, actions: ConversationRowActions) {
  const movable = actions.canMoveToFolder && conversation.isPinned !== true;
  const { listeners, setNodeRef, isDragging } = useDraggable({ id: conversation.id, disabled: !movable });
  const onPointerDown = movable ? (listeners?.['onPointerDown'] as React.PointerEventHandler | undefined) : undefined;
  return { ref: setNodeRef, onPointerDown, isDragging };
}

/** The selected row scrolls into view: after a navigation, a refetch, or a group or folder that opened for it. */
function useScrollIntoViewWhenSelected(selected: boolean) {
  const node = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (selected) node.current?.scrollIntoView?.({ block: 'nearest' });
  }, [selected]);
  return node;
}

export function ConversationRow({ conversation, selected, indent, actions }: ConversationRowProps): React.JSX.Element {
  const showMenu = useContextMenu();
  const drag = useRowDrag(conversation, actions);
  const scrollTarget = useScrollIntoViewWhenSelected(selected);
  const [renaming, setRenaming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const [moving, setMoving] = useState(false);
  const label = conversation.name === '' ? t('desktop.chats.untitled', 'Untitled chat') : conversation.name;
  const asks: RowAsks = { rename: () => setRenaming(true), move: () => setMoving(true), archive: () => setArchiving(true), remove: () => setDeleting(true) };
  const items = conversationActions(conversation, actions, asks);
  // Client contract 1.12: the activity glyph and the bold title of an unread answer.
  const activity = useRowActivity(conversation);

  return (
    <>
      {renaming ? (
        <RenameField
          conversation={conversation}
          indent={indent}
          onDone={(name) => {
            setRenaming(false);
            if (name !== null && name !== '') actions.rename(conversation, name);
          }}
        />
      ) : (
        <Box
          ref={(element: HTMLDivElement | null) => {
            drag.ref(element);
            scrollTarget.current = element;
          }}
          data-conversation-id={conversation.id}
          onPointerDown={drag.onPointerDown}
          sx={{ opacity: drag.isDragging ? 0.5 : 1 }}
        >
          <ShellRow
            testId="shell-conversation"
            label={label}
            title={label}
            indent={indent}
            selected={selected}
            onClick={() => actions.open(conversation)}
            onContextMenu={(event) => showMenu(event, items)}
            bold={activityTitleBold(activity)}
            status={<ConversationActivityGlyph kind={activityGlyph(activity)} />}
            trailingOnHover
            trailing={<RowMenu items={items} label={t('desktop.chats.more', 'More for {{name}}', { name: label })} />}
          />
        </Box>
      )}
      {moving && (
        <MoveToFolderDialog
          open
          name={label}
          folders={actions.folders}
          currentFolderId={actions.folderOf(conversation)}
          onClose={() => setMoving(false)}
          onMove={(folderId) => actions.moveToFolder(conversation, folderId)}
        />
      )}
      <BaseModal
        open={archiving}
        title={t('desktop.chats.archiveTitle', 'Archive “{{name}}”?', { name: label })}
        content={t('desktop.chats.archiveText', "The chat moves to the project's Archived chats and leaves the chat list for everyone in the project. Any member can unarchive it.")}
        onClose={() => setArchiving(false)}
        onConfirm={() => {
          setArchiving(false);
          actions.archive(conversation);
        }}
        actions={{ confirmText: t('desktop.chats.archive', 'Archive') }}
      />
      <DeleteEntityModal
        open={deleting}
        name={label}
        onClose={() => setDeleting(false)}
        onConfirm={() => {
          setDeleting(false);
          actions.remove(conversation);
        }}
        copy={{ title: t('desktop.chats.deleteTitle', 'Delete chat'), textContent: t('desktop.chats.deleteText', "This can't be undone. Delete ") }}
      />
    </>
  );
}
