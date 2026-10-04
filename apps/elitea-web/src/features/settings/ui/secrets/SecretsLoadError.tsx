/**
 * The Secrets grid's error state — what the page shows when the list query
 * FAILED, as opposed to when it settled with no secrets (UI-UX-1(b)).
 *
 * The page used to hand the table `[]` on a failed list, and the table then
 * showed its "No secrets" overlay. A project whose vault would not open (F1:
 * an integer default-model id the backend refused to decode) therefore looked
 * like a project with no secrets — and that empty grid invites exactly the
 * create that the backend's unreadable-vault guard exists to stop. A failure
 * is now said as one, with a way to try again.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { tableStyles } from './SecretsTable.styles';
import { t } from '@/shared/i18n';

export const SECRETS_LOAD_ERROR_TESTID = 'secrets-load-error';

export interface SecretsLoadErrorProps {
  /** The failure was a 403 — retrying will not help, so no retry is offered. */
  forbidden: boolean;
  /** Re-run the list query. */
  onRetry: () => void;
}

export const SecretsLoadError = memo(function SecretsLoadError({ forbidden, onRetry }: SecretsLoadErrorProps) {
  return (
    <Box role="alert" data-testid={SECRETS_LOAD_ERROR_TESTID} sx={tableStyles.loadError}>
      <Typography variant="bodyMedium" color="error">
        {forbidden
          ? t('entities.secret.error.forbidden', 'The access is not allowed')
          : t('entities.secret.error.listFailed', 'Failed to load secrets')}
      </Typography>
      {forbidden ? null : (
        <Button variant="elitea" color="secondary" onClick={onRetry}>
          {t('entities.secret.retry', 'Try again')}
        </Button>
      )}
    </Box>
  );
});
