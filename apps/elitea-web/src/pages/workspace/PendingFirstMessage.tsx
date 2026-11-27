/**
 * A new thread's first message while its thread is prepared (#3127): the message as sent, and under it "Preparing
 * workspace…" (the conversation, and a worktree when chosen, are being made), or "Not sent" with Retry when that
 * failed (the reason is said above the composer). It gives way to the turn's own prompt once the turn starts.
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { YourPrompt } from './sessionParts';
import type { FirstMessageSlot } from './useFirstSend';

export function PendingFirstMessage({ slot }: { slot: FirstMessageSlot }): React.JSX.Element | null {
  const message = slot.message;
  if (message === null) return null;
  const [prompt, , , , images] = message.args;
  const failed = message.state === 'failed';
  return (
    <Box data-testid="pending-first-message" data-state={message.state} sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <YourPrompt text={prompt} images={images?.shown} />
      <Box component="output" sx={{ alignSelf: 'flex-end', display: 'flex', alignItems: 'center', gap: 1 }}>
        {failed ? (
          <>
            <Typography variant="bodySmall" color="error">
              {t('workspace.first.notSent', 'Not sent')}
            </Typography>
            <Button size="small" onClick={slot.retry} data-testid="pending-first-message-retry">
              {t('workspace.first.retry', 'Retry')}
            </Button>
          </>
        ) : (
          <>
            <CircularProgress size={12} aria-hidden />
            <Typography variant="bodySmall" color="text.secondary">
              {t('workspace.first.preparing', 'Preparing workspace…')}
            </Typography>
          </>
        )}
      </Box>
    </Box>
  );
}
