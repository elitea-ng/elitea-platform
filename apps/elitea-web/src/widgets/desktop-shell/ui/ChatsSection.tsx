/**
 * The conversations under the sidebar's Chats row, as native rows: Pinned
 * first, then the folders, then the recent ones by date (Today, Yesterday,
 * Previous 7 days, Previous 30 days, Older — the listing's own buckets), each
 * bucket with a lazy "Show more"; an "Archived" row at the end opens the
 * archived chats page.
 *
 * One outline grid (`rows.tsx` `CHILD_INDENT`): every row under Chats has its
 * icon column at the Chats row's text edge and its label on one text edge —
 * a folder's glyph and the Archived glyph in that column, a conversation's
 * column empty — and a folder's disclosure chevron one column to the left.
 * The date-group captions sit on the same text edge and are disclosures:
 * Pinned and Today open, the older groups closed by default, each remembered
 * (`useChatGroupsOpen`); a closed group shows its count.
 *
 * The open conversation (the route) is the selected row: the group or folder
 * that holds it opens for it (until the person closes it again), and the row
 * scrolls into view.
 *
 * The data is the frame's chat rail (`model/chatRail.tsx`, the web chat
 * list's `useConversationRail`): the same grouped listing and cache, so Local
 * work threads and hidden conversations are left out exactly as the web list
 * leaves them out. Folders are managed as in the web list, through its hooks:
 * "New folder" on the Chats row, "Move to folder…" in a row's menu, and a drag
 * of a row onto a folder (or out of one onto the recent chats,
 * `useSidebarChatDrag`).
 */
import { useEffect } from 'react';

import { DndContext, pointerWithin } from '@dnd-kit/core';
import { useNavigate } from '@tanstack/react-router';

import ArchiveOutlinedIcon from '@mui/icons-material/ArchiveOutlined';
import Box from '@mui/material/Box';

import { t } from '@/shared/i18n';

import { ARCHIVED_PATH, ChatRailProvider, openConversationId, PINNED_GROUP, useChatRail, type ChatRail } from '../model/chatRail';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { useChatDisclosures } from '../model/useChatDisclosures';
import { chatGroupKey } from '../model/useEliteaSectionOpen';
import { useSidebarChatDrag } from '../model/useSidebarChatDrag';
import type { ConversationRowActions } from './ConversationRow';
import { DragPreview, FolderItem, Group, LIST_SX, NewFolderField, RecentArea, Rows, ShowMore } from './ChatsSectionParts';
import { CHILD_INDENT, ShellRow } from './rows';

/** The listing's bucket names (`folders/handler.go` `groupOrder`) as sidebar captions. */
function groupCaption(name: string): string {
  switch (chatGroupKey(name)) {
    case 'today':
      return t('desktop.chats.today', 'Today');
    case 'yesterday':
      return t('desktop.chats.yesterday', 'Yesterday');
    case 'this_week':
      return t('desktop.chats.previous7', 'Previous 7 days');
    case 'this_month':
      return t('desktop.chats.previous30', 'Previous 30 days');
    case 'older':
      return t('desktop.chats.older', 'Older');
    default:
      return name;
  }
}

/** A failure of the list, told the way the shell tells everything else (its notice banner). */
function useErrorNotice(rail: ChatRail): void {
  const { error, dismissError } = rail;
  useEffect(() => {
    if (error === undefined) return;
    useDesktopLayout.getState().setNotice(error);
    dismissError();
  }, [error, dismissError]);
}

function useRowActions(rail: ChatRail, openId: string, rights: { readonly canArchive: boolean; readonly canMoveToFolder: boolean }): ConversationRowActions {
  const navigate = useNavigate();
  return {
    open: (conversation) => void navigate({ to: '/chat/$conversationId', params: { conversationId: conversation.id } }),
    pin: rail.pin,
    rename: rail.rename,
    canModify: rail.canModify,
    canArchive: rights.canArchive,
    canMoveToFolder: rights.canMoveToFolder,
    // The open conversation stays open when it is archived (an archive changes the lists, not the chat).
    archive: rail.archive,
    remove: (conversation) =>
      void rail.remove(conversation).then((removed) => {
        if (removed && conversation.id === openId) void navigate({ to: '/chat' });
      }),
    folders: rail.folders.map((folder) => ({ id: folder.id, name: folder.name })),
    folderOf: (conversation) => rail.folders.find((folder) => folder.conversations.some((item) => item.id === conversation.id))?.id ?? null,
    moveToFolder: (conversation, folderId) => void rail.moveToFolder(conversation, folderId),
  };
}

export interface ChatsSectionProps {
  readonly projectId: string | undefined;
  readonly pathname: string;
  /** The Chats row's "New folder" was chosen: show the name field. */
  readonly creatingFolder?: boolean;
  /** The name field closed (created or cancelled). */
  readonly onCreatingFolderDone?: () => void;
  /** Holds `PERMISSIONS.chat.archive` in this project. */
  readonly canArchive?: boolean;
  /** Holds `PERMISSIONS.chat.folders.update` in this project: Move to folder… and the drag. */
  readonly canMoveToFolder?: boolean;
}

function ChatsList({ rail, pathname, creatingFolder = false, onCreatingFolderDone, canArchive = false, canMoveToFolder = false }: ChatsSectionProps & { readonly rail: ChatRail }): React.JSX.Element {
  useErrorNotice(rail);
  const openId = openConversationId(pathname);
  const actions = useRowActions(rail, openId, { canArchive, canMoveToFolder });
  const navigate = useNavigate();
  const drag = useSidebarChatDrag(rail);
  const open = useChatDisclosures(rail, openId);
  const groups = rail.dateGroups.filter((group) => group.conversations.length > 0);
  const empty = !rail.isLoading && rail.pinned.length === 0 && rail.folders.length === 0 && groups.length === 0;

  return (
    <DndContext sensors={drag.sensors} collisionDetection={pointerWithin} onDragStart={drag.handleDragStart} onDragEnd={(event) => void drag.handleDragEnd(event)} autoScroll={false}>
      <Box component="ul" data-testid="shell-chats" aria-label={t('desktop.chats.label', 'Chats')} sx={LIST_SX}>
        {rail.pinned.length > 0 && (
          <Group caption={t('desktop.chats.pinned', 'Pinned')} count={rail.pinned.length} open={open.groupOpen(PINNED_GROUP)} onToggle={() => open.toggleGroup(PINNED_GROUP)}>
            <Rows conversations={rail.pinned} openId={openId} indent={CHILD_INDENT} actions={actions} />
          </Group>
        )}
        {creatingFolder && (
          <NewFolderField
            onDone={(name) => {
              onCreatingFolderDone?.();
              if (name !== null && name !== '') void rail.createFolder(name);
            }}
          />
        )}
        {rail.folders.map((folder) => (
          <FolderItem
            key={folder.id}
            rail={rail}
            folder={folder}
            expanded={open.folderOpen(folder.id)}
            onToggle={() => open.toggleFolder(folder.id)}
            onExpand={() => open.expandFolder(folder.id)}
            openId={openId}
            actions={actions}
            drag={drag}
          />
        ))}
        <RecentArea drag={drag}>
          {groups.map((group) => (
            <Group key={group.name} caption={groupCaption(group.name)} count={group.total ?? group.conversations.length} open={open.groupOpen(group.name)} onToggle={() => open.toggleGroup(group.name)}>
              <Rows conversations={group.conversations} openId={openId} indent={CHILD_INDENT} actions={actions} />
              {rail.hasMore(group) && <ShowMore indent={CHILD_INDENT} onClick={() => rail.loadMoreInGroup(group.name)} />}
            </Group>
          ))}
        </RecentArea>
        {empty && (
          <li>
            <ShellRow muted indent={CHILD_INDENT} reserveIcon label={t('desktop.chats.empty', 'No chats yet')} onClick={() => void navigate({ to: '/chat' })} />
          </li>
        )}
        <li>
          <ShellRow
            testId="shell-chats-archived"
            muted={pathname !== ARCHIVED_PATH}
            indent={CHILD_INDENT}
            icon={<ArchiveOutlinedIcon />}
            label={t('desktop.chats.archived', 'Archived')}
            selected={pathname === ARCHIVED_PATH}
            onClick={() => void navigate({ to: ARCHIVED_PATH })}
          />
        </li>
      </Box>
      <DragPreview drag={drag} />
    </DndContext>
  );
}

/** The frame's chat rail when there is one; outside the frame (tests) a rail of its own. */
export function ChatsSection(props: ChatsSectionProps): React.JSX.Element {
  const rail = useChatRail();
  if (rail !== null) return <ChatsList {...props} rail={rail} />;
  return (
    <ChatRailProvider projectId={props.projectId} enabled>
      <OwnRail {...props} />
    </ChatRailProvider>
  );
}

function OwnRail(props: ChatsSectionProps): React.JSX.Element | null {
  const rail = useChatRail();
  return rail === null ? null : <ChatsList {...props} rail={rail} />;
}
