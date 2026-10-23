/**
 * One inline card for an authored pipeline pause (`interrupt_before` /
 * `interrupt_after`), in the generic `ChatHitlActions` card's visual
 * language (reference `ChatHitlActions.jsx` `container`/`message`/
 * `buttonContainer` styles). EliteaUI has no screen of its own for this
 * pause, so nothing new is invented: no raw thread ids or proof kinds, no
 * prefilled text box, no multi-select — each pause continues on its own.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { CheckedIcon } from '@/shared/ui/icons/checked-icon';

/** @public Props for `StaticPauseCard`. */
export interface StaticPauseCardProps {
  readonly nodeName: string;
  readonly disabled?: boolean;
  readonly error?: string | undefined;
  readonly onContinue: () => void;
}

const styles = {
  container: (theme: Theme) => ({
    width: '100%',
    display: 'flex',
    flexDirection: 'column',
    padding: '0.875rem 1rem 0.9375rem',
    gap: '0.75rem',
    borderRadius: theme.vars.shape.radiusMd,
    background: theme.vars.palette.background.chatContinueBackground,
    alignItems: 'flex-start',
  }),
  message: (theme: Theme) => ({ color: theme.vars.palette.text.secondary }),
  buttons: { display: 'flex', gap: '0.625rem', alignItems: 'center', flexWrap: 'wrap' },
} as const;

export function StaticPauseCard({ disabled = false, error, onContinue }: StaticPauseCardProps): ReactNode {
  return (
    <Box data-testid="chat-static-pause-panel" sx={styles.container}>
      <Typography variant="bodyMedium" component="p" sx={styles.message}>
        {t('features.chatMessages.staticPause.message', 'Execution paused at static interrupt. Choose the action to proceed or type the continuation message in chat.')}
      </Typography>
      {error && (
        <Typography variant="bodySmall" component="p" role="alert" sx={{ color: 'error.main' }}>
          {error}
        </Typography>
      )}
      <Box sx={styles.buttons}>
        <BaseBtn
          data-testid="chat-static-pause-continue-button"
          variant="positive"
          startIcon={<CheckedIcon />}
          onClick={onContinue}
          disabled={disabled}
        >
          {t('features.chatMessages.staticPause.continue', 'Continue')}
        </BaseBtn>
      </Box>
    </Box>
  );
}
