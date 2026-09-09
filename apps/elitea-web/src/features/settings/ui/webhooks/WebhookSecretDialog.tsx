/**
 * WebhookSecretDialog — shown once right after a webhook is created or its
 * secret rotated (#876), so the caller can copy the value they need to
 * verify the `X-Webhook-Signature` header on their receiving end.
 *
 * "Once" is a UX convention here, not a server guarantee: unlike the
 * personal-access-token flow this is modelled on, `GET`/`LIST` on this
 * resource keeps returning the live `secret` afterwards (`internal/api/
 * webhook/handler.go`'s file header documents that redaction is a separate,
 * still-open gap, #496). The table's own reveal toggle is what lets a caller
 * see it again later; this dialog only exists so a fresh value is not missed
 * the moment it is minted.
 */
import { useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';
import { handleCopy } from '@/shared/lib/clipboard';

export interface WebhookSecretDialogProps {
  readonly open: boolean;
  readonly secret: string;
  readonly onClose: () => void;
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '25rem' };
const secretInputSx: SxProps<Theme> = { '& input': { fontFamily: 'monospace' } };

export function WebhookSecretDialog({ open, secret, onClose }: WebhookSecretDialogProps) {
  const [copyLabel, setCopyLabel] = useState(() => t('entities.webhook.secretDialog.copy', 'Copy'));

  const onCopy = useCallback(() => {
    void handleCopy(secret);
    setCopyLabel(t('entities.webhook.secretDialog.copied', 'Copied!'));
    setTimeout(() => setCopyLabel(t('entities.webhook.secretDialog.copy', 'Copy')), 5000);
  }, [secret]);

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={t('entities.webhook.secretDialog.title', 'Save this secret')}
      content={
        <Box sx={contentSx}>
          <Typography variant="bodyMedium" color="warning.main">
            {t(
              'entities.webhook.secretDialog.warning',
              'Copy this value now. Sign every request you verify against it with HMAC-SHA256.',
            )}
          </Typography>
          <InputBase
            value={secret}
            slotProps={{ htmlInput: { readOnly: true, 'data-testid': 'webhook-secret-value' } }}
            sx={secretInputSx}
          />
        </Box>
      }
      actions={{ confirmText: copyLabel, cancelText: t('entities.webhook.secretDialog.done', 'Done') }}
      onConfirm={onCopy}
      data-testid="webhook-secret-dialog"
    />
  );
}
