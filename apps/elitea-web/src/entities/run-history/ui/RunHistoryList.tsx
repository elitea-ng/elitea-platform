/**
 * The conversation-list half of the run-history panel (issue #868): one row
 * per past conversation that carried this entity as its
 * `meta.single_participant` — date, a client-derived duration (see
 * `formatRunDuration`'s own doc comment for why it isn't server-computed),
 * the message count `listConversations` already counts, and (#955) the
 * VERSION that produced the run, when the caller can resolve one.
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
  /**
   * #955 — version id (as a string) -> version name, for the entity this
   * panel is scoped to. Optional: pipelines/toolkits callers that have not
   * threaded their own version list through yet simply get a row with no
   * version name, same as before this issue.
   */
  readonly versionNameById?: ReadonlyMap<string, string> | undefined;
}

const listSx: SxProps<Theme> = { maxHeight: '100%', overflowY: 'auto' };

/**
 * The run's OWN version, off `meta.single_participant.entity_settings.
 * version_id` — the value `AddParticipant` (internal/infra/db/repos/
 * conversations.go) stamps onto the conversation at the moment an
 * agent/pipeline participant joins it. `listConversations` returns `meta`
 * verbatim, so no new endpoint or column is needed to read it back; only a
 * NAME lookup (this entity's own version list) turns the id into the text
 * the case wants.
 */
function versionNameOf(row: ConversationSummary, versionNameById: ReadonlyMap<string, string> | undefined): string | undefined {
  if (versionNameById === undefined) return undefined;
  const singleParticipant = row.meta?.['single_participant'] as Record<string, unknown> | undefined;
  const entitySettings = singleParticipant?.['entity_settings'] as Record<string, unknown> | undefined;
  const versionId = entitySettings?.['version_id'];
  if (typeof versionId !== 'string' && typeof versionId !== 'number') return undefined;
  return versionNameById.get(String(versionId));
}

function secondaryOf(row: ConversationSummary, versionName: string | undefined): string {
  const duration = formatRunDuration(row.created_at, row.updated_at);
  const messages = t('entities.runHistory.list.messageCount', '{{count}} messages', {
    count: row.message_groups_count,
  });
  const parts = [row.created_at, ...(versionName === undefined ? [] : [versionName]), duration, messages];
  return parts.join(' · ');
}

export function RunHistoryList({
  rows,
  selectedId,
  onSelect,
  onRestoreConversation,
  versionNameById,
}: RunHistoryListProps): ReactNode {
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
            secondary={secondaryOf(row, versionNameOf(row, versionNameById))}
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
