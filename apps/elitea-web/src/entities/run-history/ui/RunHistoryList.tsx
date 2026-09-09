/**
 * The conversation-list half of the run-history panel (issue #868): one row
 * per past conversation that carried this entity as its
 * `meta.single_participant` — date, a client-derived duration (see
 * `formatRunDuration`'s own doc comment for why it isn't server-computed),
 * and the message count `listConversations` already counts.
 */
import type { ReactNode } from 'react';

import Button from '@mui/material/Button';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ConversationSummary } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { formatRunDuration } from '../lib/formatRunDuration';

export interface RunHistoryListProps {
  readonly rows: readonly ConversationSummary[];
  readonly selectedId: number | undefined;
  readonly onSelect: (row: ConversationSummary) => void;
  readonly onRestoreConversation?: ((conversationId: string) => void) | undefined;
}

const listSx: SxProps<Theme> = { maxHeight: '100%', overflowY: 'auto' };

function secondaryOf(row: ConversationSummary): string {
  const duration = formatRunDuration(row.created_at, row.updated_at);
  const messages = t('entities.runHistory.list.messageCount', '{{count}} messages', {
    count: row.message_groups_count,
  });
  return `${row.created_at} · ${duration} · ${messages}`;
}

export function RunHistoryList({ rows, selectedId, onSelect, onRestoreConversation }: RunHistoryListProps): ReactNode {
  return (
    <List dense sx={listSx} data-testid="run-history-list">
      {rows.map((row) => (
        <ListItemButton
          key={row.id}
          selected={row.id === selectedId}
          data-testid="run-history-row"
          data-conversation-id={row.id}
          onClick={() => onSelect(row)}
        >
          <ListItemText
            primary={row.name}
            secondary={secondaryOf(row)}
            slotProps={{ secondary: { variant: 'bodySmall' } }}
          />
          {onRestoreConversation !== undefined && (
            <Button
              size="small"
              data-testid="run-history-restore-button"
              onClick={(event) => {
                event.stopPropagation();
                onRestoreConversation(String(row.id));
              }}
            >
              <Typography variant="labelSmall">{t('entities.runHistory.list.restore', 'Restore')}</Typography>
            </Button>
          )}
        </ListItemButton>
      ))}
    </List>
  );
}
