/**
 * The conversations under the sidebar's Chats row, as native rows: Pinned
 * first, then the folders, then the recent ones by date (Today, Yesterday,
 * Previous 7 days, Previous 30 days, Older — the listing's own buckets), each
 * bucket with a lazy "Show more"; an "Archived" row at the end opens the
 * archived chats page.
 *
 * The grid is Local work's (`LocalWorkSection.tsx`): a folder, a date group
 * (Pinned, Today, Yesterday, Previous 7 days, Previous 30 days, Older) and
 * Archived are top-level rows with their chevron or glyph in the icon column
 * and the label on the Chats row's text edge; their conversations sit one
 * level in (`CHILD_INDENT`), on that same text edge. Pinned and Today open,
 * the older groups closed by default, each remembered (`useChatGroupsOpen`);
 * a closed group or folder shows its count.
 *
 * The open conversation (the route) is the selected row: the group or folder
 * that holds it opens for it (until the person closes it again), and the row
 * scrolls into view.
 *
 * The data is the shell's chat rail (`model/chatRail.tsx`,
 * `useConversationRail`): one grouped listing and cache, Local work threads
 * and hidden conversations left out. Folders: "New folder" on the Chats row,
 * Rename / Pin / Delete in a folder's menu, "Move to folder…" in a row's menu,
 * and a drag of a row onto a folder (or out of one onto the recent chats,
 * `useSidebarChatDrag`). The Chats caption's search opens a title filter at
 * the top of the list (`ChatsSearchField`).
 *
 * The same list serves the web and the desktop. What only the desktop has
 * comes in as props: `onError` (its notice banner; the web shows a snackbar
 * of the list's own) and `onNativeContextMenu` (a right-click opens the OS
 * menu; the web keeps the browser's).
 */
import { useCallback, useEffect, useState } from 'react';

import { DndContext, pointerWithin } from '@dnd-kit/core';
import { useNavigate } from '@tanstack/react-router';

import ArchiveOutlinedIcon from '@mui/icons-material/ArchiveOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Snackbar from '@mui/material/Snackbar';

import { t } from '@/shared/i18n';

import { ARCHIVED_PATH, ChatRailProvider, openConversationId, PINNED_GROUP, useChatRail, useChatRailFilter, type ChatRail } from '../model/chatRail';
import { useChatDisclosures } from '../model/useChatDisclosures';
import { useRowActions } from '../model/useRowActions';
import { chatGroupKey } from '../model/useSectionOpen';
import { useSidebarChatDrag } from '../model/useSidebarChatDrag';
import { ChatsSearchField } from './ChatsSearchField';
import { DragPreview, FolderItem, FolderNameField, Group, LIST_SX, RecentArea, Rows, ShowMore, type RowEnv } from './ChatsSectionParts';
import type { NativeContextMenu } from './ConversationRow';
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

/** A failure of the list, told through `onError` (the desktop's notice banner) or, without one, the list's own snackbar. */
function useErrorReport(rail: ChatRail, onError: ((message: string) => void) | undefined) {
  const [shown, setShown] = useState<string | undefined>(undefined);
  const report = useCallback((message: string) => (onError === undefined ? setShown(message) : onError(message)), [onError]);
  const { error, dismissError } = rail;
  useEffect(() => {
    if (error === undefined) return;
    report(error);
    dismissError();
  }, [error, dismissError, report]);
  return { report, shown, dismiss: () => setShown(undefined) };
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
  /** Holds `PERMISSIONS.chat.folders.update` in this project: Move to folder…, the drag, a folder's Rename and Pin. */
  readonly canMoveToFolder?: boolean;
  /** Holds `PERMISSIONS.chat.folders.delete` in this project. */
  readonly canDeleteFolder?: boolean;
  /** Tells a failure; without it the list shows a snackbar of its own. */
  readonly onError?: ((message: string) => void) | undefined;
  /** The desktop's native context menu for a row. */
  readonly onNativeContextMenu?: NativeContextMenu | undefined;
}

/** What every row of the list shares: its actions, the project, the native menu, the Local work filter. */
function useRowEnv(props: ChatsSectionProps, rail: ChatRail, openId: string, report: (message: string) => void): RowEnv {
  const filter = useChatRailFilter();
  const can = { canArchive: props.canArchive === true, canMoveToFolder: props.canMoveToFolder === true };
  return {
    actions: useRowActions(rail, openId, props.projectId, can, report),
    projectId: props.projectId,
    onNativeContextMenu: props.onNativeContextMenu,
    localWork: filter?.localWorkOnly === true,
  };
}

/** The list's last rows: "No chats yet" (or "No chats match" while searching) and the Archived row. */
function TailRows({ empty, searching, pathname }: { readonly empty: boolean; readonly searching: boolean; readonly pathname: string }): React.JSX.Element {
  const navigate = useNavigate();
  return (
    <>
      {empty && (
        <li>
          <ShellRow
            muted
            testId="shell-chats-empty"
            indent={CHILD_INDENT}
            label={searching ? t('desktop.chats.noMatch', 'No chats match') : t('desktop.chats.empty', 'No chats yet')}
            onClick={() => void navigate({ to: '/chat' })}
          />
        </li>
      )}
      <li>
        <ShellRow
          testId="shell-chats-archived"
          muted={pathname !== ARCHIVED_PATH}
          icon={<ArchiveOutlinedIcon />}
          label={t('desktop.chats.archived', 'Archived')}
          selected={pathname === ARCHIVED_PATH}
          onClick={() => void navigate({ to: ARCHIVED_PATH })}
        />
      </li>
    </>
  );
}

function ChatsList(props: ChatsSectionProps & { readonly rail: ChatRail }): React.JSX.Element {
  const { rail, pathname } = props;
  const errors = useErrorReport(rail, props.onError);
  const filter = useChatRailFilter();
  const openId = openConversationId(pathname);
  const env = useRowEnv(props, rail, openId, errors.report);
  const drag = useSidebarChatDrag(rail, errors.report);
  const open = useChatDisclosures(rail, openId);
  const groups = rail.dateGroups.filter((group) => group.conversations.length > 0);
  const empty = !rail.isLoading && rail.pinned.length === 0 && rail.folders.length === 0 && groups.length === 0;
  const searchOpen = filter?.searchOpen === true;
  const searching = searchOpen && filter.query.trim() !== '';

  return (
    <DndContext sensors={drag.sensors} collisionDetection={pointerWithin} onDragStart={drag.handleDragStart} onDragEnd={(event) => void drag.handleDragEnd(event)} autoScroll={false}>
      {searchOpen && <ChatsSearchField filter={filter} />}
      <Box component="ul" data-testid="shell-chats" aria-label={t('desktop.chats.label', 'Chats')} sx={LIST_SX}>
        {rail.pinned.length > 0 && (
          <Group caption={t('desktop.chats.pinned', 'Pinned')} count={rail.pinned.length} rows={rail.pinned} open={open.groupOpen(PINNED_GROUP)} onToggle={() => open.toggleGroup(PINNED_GROUP)}>
            <Rows conversations={rail.pinned} openId={openId} indent={CHILD_INDENT} env={env} />
          </Group>
        )}
        {props.creatingFolder === true && (
          <FolderNameField
            onDone={(name) => {
              props.onCreatingFolderDone?.();
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
            env={env}
            drag={drag}
            canUpdateFolder={props.canMoveToFolder === true}
            canDeleteFolder={props.canDeleteFolder === true}
          />
        ))}
        <RecentArea drag={drag}>
          {groups.map((group) => (
            <Group key={group.name} caption={groupCaption(group.name)} count={group.total ?? group.conversations.length} rows={group.conversations} serverNeedsYou={group.needsYou} open={open.groupOpen(group.name)} onToggle={() => open.toggleGroup(group.name)}>
              <Rows conversations={group.conversations} openId={openId} indent={CHILD_INDENT} env={env} />
              {rail.hasMore(group) && <ShowMore indent={CHILD_INDENT} onClick={() => rail.loadMoreInGroup(group.name)} />}
            </Group>
          ))}
        </RecentArea>
        <TailRows empty={empty} searching={searching} pathname={pathname} />
      </Box>
      <DragPreview drag={drag} />
      <Snackbar open={errors.shown !== undefined} autoHideDuration={6000} onClose={errors.dismiss} anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}>
        <Alert severity="error" variant="filled" onClose={errors.dismiss}>
          {errors.shown}
        </Alert>
      </Snackbar>
    </DndContext>
  );
}

/** The shell's chat rail when there is one; outside a provider (tests) a rail of its own. */
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
