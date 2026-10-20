/**
 * The conversations under the sidebar's Chats row, as native rows: Pinned
 * first, then the folders, then the recent ones by date (Today, Yesterday,
 * Previous 7 days, Previous 30 days, Older — the listing's own buckets), each
 * bucket with a lazy "Show more"; the open conversation (the route) is
 * highlighted; an "Archived" row at the end opens the archived chats page.
 *
 * The data is the web chat list's (`useConversationRail` over
 * `features/chat-conversation-list`): the same grouped listing and cache, so
 * Local work threads and hidden conversations are left out exactly as the
 * web list leaves them out. While this list is mounted it tells the frame so
 * (`shared/desktop/sidebarChatList`), and the chat page drops its own in-page
 * list; when it is not (the sidebar hidden, a section collapsed) the page
 * shows its list again.
 *
 * Folders are managed here as in the web list, through its hooks: the Chats
 * row's "New folder" opens a name field at the top of the folders, and a
 * row's "Move to folder…" moves the conversation.
 */
import { useEffect, useState, type KeyboardEvent } from 'react';

import { useNavigate } from '@tanstack/react-router';

import ArchiveOutlinedIcon from '@mui/icons-material/ArchiveOutlined';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { Conversation } from '@/entities/conversation';
import { useConversationRail } from '@/features/chat-conversation-list';
import { useAnnounceSidebarChatList } from '@/shared/desktop/sidebarChatList';
import { t } from '@/shared/i18n';
import { isLocalWorkConversation } from '@/shared/lib/localWork';

import { useDesktopLayout } from '../model/desktopLayout.store';
import { ConversationRow, type ConversationRowActions } from './ConversationRow';
import { CHILD_INDENT, ShellRow } from './rows';

const ARCHIVED_PATH = '/chat/archived';

/** The listing's bucket names (`folders/handler.go` `groupOrder`) as sidebar captions. */
function groupCaption(name: string): string {
  switch (name.toLowerCase().replaceAll(' ', '_')) {
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

/** The open conversation, from the route (`/chat/<id>`); `''` elsewhere. */
function openConversationId(pathname: string): string {
  const match = /^\/chat\/([^/]+)\/?$/.exec(pathname);
  return match?.[1] === undefined || pathname.startsWith(ARCHIVED_PATH) ? '' : decodeURIComponent(match[1]);
}

function GroupCaption({ children }: { readonly children: string }): React.JSX.Element {
  return (
    <Typography
      component="h3"
      variant="bodySmall"
      data-testid="shell-chats-group"
      sx={(theme: Theme) => ({ margin: 0, paddingLeft: 1 + CHILD_INDENT, paddingTop: 1, paddingBottom: 0.25, fontWeight: 600, color: theme.vars.palette.text.metrics })}
    >
      {children}
    </Typography>
  );
}

interface ListProps {
  readonly conversations: readonly Conversation[];
  readonly openId: string;
  readonly indent: number;
  readonly actions: ConversationRowActions;
}

function Rows({ conversations, openId, indent, actions }: ListProps): React.JSX.Element {
  // The listing already leaves Local work threads out; a row that still says so belongs to the Local work section above.
  return (
    <>
      {conversations.filter((conversation) => !isLocalWorkConversation(conversation)).map((conversation) => (
        <li key={conversation.id}>
          <ConversationRow conversation={conversation} selected={conversation.id === openId} indent={indent} actions={actions} />
        </li>
      ))}
    </>
  );
}

function ShowMore({ indent, onClick }: { readonly indent: number; readonly onClick: () => void }): React.JSX.Element {
  return (
    <li>
      <ShellRow testId="shell-chats-more" muted indent={indent} label={t('desktop.chats.showMore', 'Show more')} onClick={onClick} />
    </li>
  );
}

type Rail = ReturnType<typeof useConversationRail>;

function Folders({ rail, openId, actions }: { readonly rail: Rail; readonly openId: string; readonly actions: ConversationRowActions }): React.JSX.Element {
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const toggle = (id: string): void =>
    setOpen((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  return (
    <>
      {rail.folders.map((folder) => {
        const expanded = open.has(folder.id) || folder.conversations.some((conversation) => conversation.id === openId);
        return (
          <li key={folder.id}>
            <ShellRow testId="shell-chats-folder" indent={CHILD_INDENT} icon={expanded ? <ExpandMoreIcon /> : <ChevronRightIcon />} ariaExpanded={expanded} label={folder.name} onClick={() => toggle(folder.id)} />
            {expanded && (
              <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
                <Rows conversations={folder.conversations} openId={openId} indent={CHILD_INDENT * 2} actions={actions} />
                {rail.hasMore(folder) && <ShowMore indent={CHILD_INDENT * 2} onClick={() => rail.loadMoreInFolder(folder.id)} />}
              </Box>
            )}
          </li>
        );
      })}
    </>
  );
}

/** A failure of the list, told the way the shell tells everything else (its notice banner). */
function useErrorNotice(rail: Rail): void {
  const { error, dismissError } = rail;
  useEffect(() => {
    if (error === undefined) return;
    useDesktopLayout.getState().setNotice(error);
    dismissError();
  }, [error, dismissError]);
}

/** The new folder's name: Enter creates it, Escape or leaving the field cancels. */
function NewFolderField({ onDone }: { readonly onDone: (name: string | null) => void }): React.JSX.Element {
  const [value, setValue] = useState('');
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter') onDone(value.trim());
    else if (event.key === 'Escape') onDone(null);
  };
  return (
    <Box component="li" sx={{ paddingLeft: 1 + CHILD_INDENT, paddingRight: 0.5, minHeight: '1.75rem', display: 'flex', alignItems: 'center' }}>
      <InputBase
        value={value}
        placeholder={t('desktop.chats.newFolderPlaceholder', 'Folder name')}
        onChange={(event) => setValue(event.target.value)}
        onKeyDown={onKeyDown}
        onBlur={() => onDone(null)}
        inputRef={(node: HTMLInputElement | null) => node?.focus()}
        inputProps={{ 'aria-label': t('desktop.chats.newFolderLabel', 'New folder name'), 'data-testid': 'shell-chats-new-folder-name' }}
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

function useRowActions(rail: Rail, openId: string, canArchive: boolean): ConversationRowActions {
  const navigate = useNavigate();
  return {
    open: (conversation) => void navigate({ to: '/chat/$conversationId', params: { conversationId: conversation.id } }),
    pin: rail.pin,
    rename: rail.rename,
    canModify: rail.canModify,
    canArchive,
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
}

export function ChatsSection({ projectId, pathname, creatingFolder = false, onCreatingFolderDone, canArchive = false }: ChatsSectionProps): React.JSX.Element {
  const rail = useConversationRail({ projectId });
  useAnnounceSidebarChatList();
  useErrorNotice(rail);
  const openId = openConversationId(pathname);
  const actions = useRowActions(rail, openId, canArchive);
  const navigate = useNavigate();
  const groups = rail.dateGroups.filter((group) => group.conversations.length > 0);
  const empty = !rail.isLoading && rail.pinned.length === 0 && rail.folders.length === 0 && groups.length === 0;

  return (
    <Box component="ul" data-testid="shell-chats" aria-label={t('desktop.chats.label', 'Chats')} sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
      {rail.pinned.length > 0 && (
        <li>
          <GroupCaption>{t('desktop.chats.pinned', 'Pinned')}</GroupCaption>
          <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
            <Rows conversations={rail.pinned} openId={openId} indent={CHILD_INDENT} actions={actions} />
          </Box>
        </li>
      )}
      {creatingFolder && (
        <NewFolderField
          onDone={(name) => {
            onCreatingFolderDone?.();
            if (name !== null && name !== '') void rail.createFolder(name);
          }}
        />
      )}
      <Folders rail={rail} openId={openId} actions={actions} />
      {groups.map((group) => (
        <li key={group.name}>
          <GroupCaption>{groupCaption(group.name)}</GroupCaption>
          <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
            <Rows conversations={group.conversations} openId={openId} indent={CHILD_INDENT} actions={actions} />
            {rail.hasMore(group) && <ShowMore indent={CHILD_INDENT} onClick={() => rail.loadMoreInGroup(group.name)} />}
          </Box>
        </li>
      ))}
      {empty && (
        <li>
          <ShellRow muted indent={CHILD_INDENT} label={t('desktop.chats.empty', 'No chats yet')} onClick={() => void navigate({ to: '/chat' })} />
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
  );
}
