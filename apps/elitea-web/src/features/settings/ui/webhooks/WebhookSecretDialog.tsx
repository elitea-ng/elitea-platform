/**
 * WebhookSecretDialog — shown once right after a webhook is created or its
 * secret rotated (#876), so the caller can copy the value they need to
 * verify the `X-Webhook-Signature` header on their receiving end.
 *
 * "Once" is a server guarantee (issue 2963): the secret is stored in the
 * project vault, and only the create and rotate responses carry it. No read
 * returns it, so this dialog is the caller's one chance to copy the value;
 * a lost value is replaced by rotating, never read back.
 */
import { useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';
import { handleCopy } from '@/shared/lib/clipboard';
import { monoFontFamily } from '@/shared/brand/typeScale';

export interface WebhookSecretDialogProps {
  readonly open: boolean;
  readonly secret: string;
  readonly onClose: () => void;
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '25rem' };
const secretInputSx: SxProps<Theme> = { '& input': { fontFamily: monoFontFamily } };

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
