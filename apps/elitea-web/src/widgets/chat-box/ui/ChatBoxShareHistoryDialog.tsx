/**
 * "Share history from" (ADR-0050 decision 9b, issue 1669): an agent added to
 * a conversation that already has messages reads it from the start, or only
 * from now on. "Now" is `entity_settings.context_history = since_joined`; the
 * server dates it from the add, so the agent's next answer reads only the
 * messages after it joined.
 *
 * Its own file because `ChatBox.tsx` is at its §3.5 file-length ceiling (#1808).
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { ShareHistoryQuestion } from './hooks/useAddEntityParticipant';

export function ChatBoxShareHistoryDialog({ question }: { readonly question: ShareHistoryQuestion }): ReactNode {
  if (!question.open) return null;
  return (
    <BaseModal
      open
      title={t('widgets.chatBox.shareHistory.title', 'Share history from')}
      data-testid="share-history-dialog"
      onClose={question.onCancel}
      content={
        <Typography variant="bodyMedium">
          {t(
            'widgets.chatBox.shareHistory.body',
            'This conversation already has messages. Should the agent read them, or only what is said from now on?',
          )}
        </Typography>
      }
      actions={{
        node: (
          <Box sx={{ display: 'flex', gap: 1 }}>
            <BaseBtn variant="secondary" data-testid="share-history-now" onClick={() => question.onChoose('now')}>
              {t('widgets.chatBox.shareHistory.now', 'Now')}
            </BaseBtn>
            <BaseBtn variant="contained" data-testid="share-history-start" onClick={() => question.onChoose('start')}>
              {t('widgets.chatBox.shareHistory.start', 'The start')}
            </BaseBtn>
          </Box>
        ),
      }}
    />
  );
}
