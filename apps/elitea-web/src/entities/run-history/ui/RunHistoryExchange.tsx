/**
 * The whole exchange of the selected run, user message included (#1981).
 *
 * The trace below it lists the worker's steps (thinking, tool calls, the
 * model's text) and says nothing of what was asked. This reads the same
 * conversation's messages through `getConversation` (`messages_limit`, oldest
 * first) and shows them in order, so the run reads as a conversation again.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useGetConversation } from '@/shared/api/generated/chat/chat';
import type { ConversationDetail } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

import { exchangeOf } from '../lib/runExchange';

/** The oldest groups the server embeds, capped; a run is a handful of turns. */
const EXCHANGE_MESSAGES = 50;

const rootSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem', marginBottom: '1rem' };
const entrySx: SxProps<Theme> = { whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' };

export function RunHistoryExchange({ projectId, conversationId }: { readonly projectId: string; readonly conversationId: string }): ReactNode {
  const query = useGetConversation(projectId, conversationId, { messages_limit: EXCHANGE_MESSAGES, sort_order: 'asc' });
  if (query.isPending) {
    return (
      <Box data-testid="run-history-exchange-loading">
        <CircularProgress size={20} />
      </Box>
    );
  }
  const detail = unwrapBody(query.data) as ConversationDetail | undefined;
  const entries = exchangeOf(detail?.message_groups);
  if (entries.length === 0) return null;
  return (
    <Box sx={rootSx} data-testid="run-history-exchange">
      {entries.map((entry) => (
        <Box key={entry.id} data-testid="run-history-exchange-entry" data-role={entry.role}>
          <Typography variant="labelSmall" color="text.secondary">
            {entry.role === 'user' ? t('entities.runHistory.exchange.user', 'You') : t('entities.runHistory.exchange.assistant', 'Agent')}
          </Typography>
          <Typography variant="bodyMedium" sx={entrySx}>
            {entry.text}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}
