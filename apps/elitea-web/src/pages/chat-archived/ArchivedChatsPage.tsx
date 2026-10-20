/**
 * `/chat/archived` — the project's archived conversations (client contract
 * 1.10). The archive is shared by the project, as the pin is: an archived
 * chat is gone from the chat lists of every member, and any member can
 * unarchive it. Each row tells who archived it and when, opens its
 * conversation (an archived conversation stays usable), and offers
 * Unarchive (back to the lists) and Delete. A name filter narrows the
 * loaded rows.
 *
 * The web chat list's header and the desktop sidebar's "Archived" row both
 * open this page.
 */
import { useState, type ReactNode } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { formatDistanceToNow } from 'date-fns';

import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import UnarchiveOutlinedIcon from '@mui/icons-material/UnarchiveOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import List from '@mui/material/List';
import ListItem from '@mui/material/ListItem';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Snackbar from '@mui/material/Snackbar';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { ConversationSummary } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { useSelectedProject } from '@/widgets/app-shell';

import { useArchivedConversations, type ArchivedConversations } from './useArchivedConversations';

/** "Archived by Ana · 3 hours ago"; a part the row does not carry is left out. */
function archivedLine(row: Pick<ConversationSummary, 'archived_by' | 'archived_at'>): string {
  const date = row.archived_at === undefined ? undefined : new Date(row.archived_at);
  const when = date === undefined || Number.isNaN(date.getTime()) ? '' : formatDistanceToNow(date, { addSuffix: true });
  const name = row.archived_by?.name ?? '';
  const who = name === '' ? t('pages.chatArchived.archived', 'Archived') : t('pages.chatArchived.archivedBy', 'Archived by {{name}}', { name });
  return when === '' ? who : `${who} · ${when}`;
}

function ArchivedRow({ row, onOpen, onUnarchive, onDelete }: { readonly row: ConversationSummary; readonly onOpen: () => void; readonly onUnarchive: () => void; readonly onDelete: () => void }): ReactNode {
  const unarchive = t('pages.chatArchived.unarchive', 'Unarchive');
  const remove = t('pages.chatArchived.delete', 'Delete');
  return (
    <ListItem
      disablePadding
      data-testid="archived-conversation"
      secondaryAction={
        <Box sx={{ display: 'flex', gap: 0.5 }}>
          <Tooltip title={unarchive}>
            <IconButton size="small" aria-label={`${unarchive} ${row.name}`} onClick={onUnarchive} data-testid="archived-conversation-unarchive">
              <UnarchiveOutlinedIcon fontSize="small" />
            </IconButton>
          </Tooltip>
          <Tooltip title={remove}>
            <IconButton size="small" aria-label={`${remove} ${row.name}`} onClick={onDelete} data-testid="archived-conversation-delete">
              <DeleteOutlineIcon fontSize="small" />
            </IconButton>
          </Tooltip>
        </Box>
      }
    >
      <ListItemButton onClick={onOpen} sx={{ paddingRight: 12 }}>
        <ListItemText primary={row.name === '' ? t('pages.chatArchived.untitled', 'Untitled conversation') : row.name} secondary={archivedLine(row)} />
      </ListItemButton>
    </ListItem>
  );
}

function ArchivedList({ archived, query, onOpen, onDelete }: { readonly archived: ArchivedConversations; readonly query: string; readonly onOpen: (row: ConversationSummary) => void; readonly onDelete: (row: ConversationSummary) => void }): ReactNode {
  const needle = query.trim().toLowerCase();
  const rows = needle === '' ? archived.rows : archived.rows.filter((row) => row.name.toLowerCase().includes(needle));
  if (archived.isLoading) return <Typography variant="bodyMedium" color="text.secondary">{t('pages.chatArchived.loading', 'Loading archived chats…')}</Typography>;
  if (archived.isError) return <Alert severity="error">{t('pages.chatArchived.loadFailed', 'The archived chats could not be loaded.')}</Alert>;
  if (rows.length === 0) {
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="archived-empty">
        {needle === '' ? t('pages.chatArchived.empty', 'No archived chats. Archive a chat from its menu to move it here for everyone in the project.') : t('pages.chatArchived.noMatch', 'No archived chat matches the filter.')}
      </Typography>
    );
  }
  return (
    <>
      <List dense data-testid="archived-list">
        {rows.map((row) => (
          <ArchivedRow key={row.id} row={row} onOpen={() => onOpen(row)} onUnarchive={() => archived.unarchive(row)} onDelete={() => onDelete(row)} />
        ))}
      </List>
      {archived.hasMore && (
        <Button size="small" disabled={archived.isFetchingMore} onClick={archived.loadMore}>
          {t('pages.chatArchived.showMore', 'Show more')}
        </Button>
      )}
    </>
  );
}

export default function ArchivedChatsPage(): ReactNode {
  const navigate = useNavigate();
  const { project } = useSelectedProject();
  const archived = useArchivedConversations(project?.id === undefined ? undefined : String(project.id));
  const [query, setQuery] = useState('');
  const [deleting, setDeleting] = useState<ConversationSummary | null>(null);

  return (
    <Box data-testid="archived-chats-page" sx={{ display: 'flex', flexDirection: 'column', gap: 2, padding: 3, maxWidth: '48rem', boxSizing: 'border-box' }}>
      <Box>
        <Button size="small" startIcon={<ArrowBackIcon />} onClick={() => void navigate({ to: '/chat' })}>
          {t('pages.chatArchived.back', 'Chats')}
        </Button>
      </Box>
      <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1 }}>
        <Typography variant="headingMedium" component="h1">
          {t('pages.chatArchived.title', 'Archived chats')}
        </Typography>
        {archived.total > 0 && <Typography variant="bodyMedium" color="text.secondary">{archived.total}</Typography>}
      </Box>
      <SimpleSearchBar value={query} onChange={setQuery} onClear={() => setQuery('')} placeholder={t('pages.chatArchived.filter', 'Filter archived chats…')} data-testid="archived-filter" />
      <ArchivedList
        archived={archived}
        query={query}
        onOpen={(row) => void navigate({ to: '/chat/$conversationId', params: { conversationId: String(row.id) } })}
        onDelete={setDeleting}
      />
      <DeleteEntityModal
        open={deleting !== null}
        name={deleting?.name ?? ''}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          const row = deleting;
          setDeleting(null);
          if (row !== null) void archived.remove(row);
        }}
        copy={{ title: t('pages.chatArchived.deleteTitle', 'Delete conversation'), textContent: t('pages.chatArchived.deleteText', "It can't be restored. Delete ") }}
      />
      <Snackbar open={archived.error !== undefined} autoHideDuration={6000} onClose={archived.dismissError}>
        <Alert severity="error" variant="filled" onClose={archived.dismissError}>
          {archived.error}
        </Alert>
      </Snackbar>
    </Box>
  );
}
