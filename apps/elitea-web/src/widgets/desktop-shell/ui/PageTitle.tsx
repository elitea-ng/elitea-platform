/**
 * The main title row's name of what is open, as the Local work toolbar names
 * its thread (folder ▸ thread): `Chats ▸ folder ▸ conversation` on a chat,
 * `Chats ▸ Archived` on the archived chats, and the page's own name (Agents,
 * Artifacts, Settings, …) elsewhere.
 *
 * The conversation's name and folder come from the frame's chat rail (the
 * rows the sidebar selects), falling back to the chat page's own read of the
 * conversation while the rail does not hold it. A double-click on the name
 * renames it in place (Enter saves, Escape cancels) when the person may
 * rename it — the sidebar's Rename, through the same rail.
 */
import { useState, type KeyboardEvent, type ReactNode } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { conversationApi } from '@/entities/conversation';
import { t } from '@/shared/i18n';

import { selectedEliteaItem, type EliteaItem } from '../lib/eliteaSections';
import { ARCHIVED_PATH, findInRail, openConversationId, useChatRail } from '../model/chatRail';

const ONE_LINE = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

function Separator(): React.JSX.Element {
  return <ChevronRightIcon aria-hidden sx={(theme: Theme) => ({ fontSize: 14, color: theme.vars.palette.text.metrics, flexShrink: 0, marginX: -0.25 })} />;
}

function Crumb({ children, strong = false }: { readonly children: ReactNode; readonly strong?: boolean }): React.JSX.Element {
  return (
    <Typography
      variant="labelMedium"
      component="span"
      sx={(theme: Theme) => ({ ...ONE_LINE, flexShrink: strong ? 1 : 0, minWidth: strong ? 0 : undefined, color: strong ? theme.vars.palette.text.secondary : theme.vars.palette.text.metrics })}
    >
      {children}
    </Typography>
  );
}

function RenameField({ name, onDone }: { readonly name: string; readonly onDone: (name: string | null) => void }): React.JSX.Element {
  const [value, setValue] = useState(name);
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter') onDone(value.trim());
    else if (event.key === 'Escape') onDone(null);
  };
  return (
    <InputBase
      value={value}
      onChange={(event) => setValue(event.target.value)}
      onKeyDown={onKeyDown}
      onBlur={() => onDone(null)}
      inputRef={(node: HTMLInputElement | null) => node?.focus()}
      inputProps={{ 'aria-label': t('desktop.chats.renameLabel', 'Conversation name'), 'data-testid': 'title-rename' }}
      sx={(theme: Theme) => ({
        flex: 1,
        minWidth: 0,
        maxWidth: '24rem',
        paddingX: 0.5,
        borderRadius: theme.vars.shape.radiusSm,
        border: `1px solid ${theme.vars.palette.primary.main}`,
        background: theme.vars.palette.background.default,
        typography: 'labelMedium',
      })}
    />
  );
}

/** The open conversation's name and folder, from the rail, else from the chat page's details read. */
function useOpenConversation(projectId: string | undefined, conversationId: string) {
  const rail = useChatRail();
  const place = findInRail(rail, conversationId);
  const details = conversationApi.useDetails({ projectId: projectId ?? '', id: conversationId }, { enabled: place === undefined && projectId !== undefined && conversationId !== '' });
  const fetchedName = (details.data as { readonly name?: unknown } | undefined)?.name;
  const name = place?.conversation.name ?? (typeof fetchedName === 'string' ? fetchedName : '');
  return { rail, place, name };
}

function ConversationTitle({ projectId, conversationId }: { readonly projectId: string | undefined; readonly conversationId: string }): React.JSX.Element {
  const { rail, place, name } = useOpenConversation(projectId, conversationId);
  const [renaming, setRenaming] = useState(false);
  const canRename = rail !== null && place !== undefined && rail.canModify(place.conversation);
  const label = name === '' ? t('desktop.chats.untitled', 'Untitled chat') : name;
  return (
    <>
      <Crumb>{t('desktop.chats.label', 'Chats')}</Crumb>
      <Separator />
      {place?.folder !== undefined && (
        <>
          <Crumb>{place.folder.name}</Crumb>
          <Separator />
        </>
      )}
      {renaming && place !== undefined && rail !== null ? (
        <RenameField
          name={place.conversation.name}
          onDone={(next) => {
            setRenaming(false);
            if (next !== null && next !== '') rail.rename(place.conversation, next);
          }}
        />
      ) : (
        <Box
          component="span"
          data-testid="title-conversation"
          title={canRename ? t('desktop.chats.renameHint', 'Double-click to rename') : label}
          onDoubleClick={canRename ? () => setRenaming(true) : undefined}
          sx={{ display: 'flex', minWidth: 0, cursor: canRename ? 'text' : undefined }}
        >
          <Crumb strong>{label}</Crumb>
        </Box>
      )}
    </>
  );
}

function pageName(pathname: string, items: readonly EliteaItem[]): string {
  if (pathname === '/settings' || pathname.startsWith('/settings/')) return t('desktop.shell.settings', 'Settings');
  const item = items.find((candidate) => candidate.value === selectedEliteaItem(pathname, items));
  return item?.label ?? '';
}

export interface PageTitleProps {
  readonly pathname: string;
  readonly items: readonly EliteaItem[];
  readonly projectId: string | undefined;
}

export function PageTitle({ pathname, items, projectId }: PageTitleProps): React.JSX.Element {
  const conversationId = openConversationId(pathname);
  const onChat = pathname === '/chat' || pathname.startsWith('/chat/');
  return (
    <Box component="h1" data-testid="page-title" sx={{ margin: 0, display: 'flex', alignItems: 'center', gap: 0.75, minWidth: 0, paddingX: 0.5 }}>
      {onChat && conversationId !== '' ? (
        <ConversationTitle projectId={projectId} conversationId={conversationId} />
      ) : onChat ? (
        <>
          <Crumb strong={pathname !== ARCHIVED_PATH}>{t('desktop.chats.label', 'Chats')}</Crumb>
          {pathname === ARCHIVED_PATH && (
            <>
              <Separator />
              <Crumb strong>{t('desktop.chats.archived', 'Archived')}</Crumb>
            </>
          )}
        </>
      ) : (
        <Crumb strong>{pageName(pathname, items)}</Crumb>
      )}
    </Box>
  );
}
