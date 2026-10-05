import { useCallback, useState } from 'react';
import type { ChangeEvent } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';

import type { CatalogApplication } from '../../model/types';

const contentSx = {
  display: 'flex',
  flexDirection: 'column' as const,
  gap: '1.25rem',
};

function descriptionSx(theme: Theme) {
  return {
    color: theme.vars.palette.text.secondary,
  };
}

const fieldWrapperSx = {
  display: 'flex',
  flexDirection: 'column' as const,
  gap: '0.5rem',
};

const actionsSx = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'flex-end',
  gap: '0.5rem',
};

export interface RequestAccessModalProps {
  open: boolean;
  application: CatalogApplication | null;
  isSubmitting: boolean;
  onClose: () => void;
  onSubmit: (application: CatalogApplication, reason: string) => void;
}

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/apps/ui/catalog/RequestAccessModal.jsx`.
 *
 * Uses `actions.node` (a full override) rather than `BaseModal`'s built-in
 * Cancel/Confirm pair: the baseline disables "Send Request" on
 * `isSubmitting || !reason.trim()` — `BaseModal`'s own action bar only
 * exposes a `confirming` flag, not an arbitrary extra disable condition
 * (see that component's own props doc) — so the exact dual condition needs
 * this component's own buttons.
 */
export function RequestAccessModal({ application, isSubmitting, onClose, onSubmit, open }: RequestAccessModalProps) {
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');

  const handleReasonChange = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      setReason(event.target.value);
      if (error) setError('');
    },
    [error],
  );

  const handleSubmit = useCallback(() => {
    if (!application) return;
    const trimmed = reason.trim();
    if (!trimmed) {
      setError(t('apps.requestAccessModal.reasonRequired', 'Please provide a reason for your request'));
      return;
    }
    onSubmit(application, trimmed);
    setReason('');
    setError('');
  }, [application, onSubmit, reason]);

  const handleClose = useCallback(() => {
    setReason('');
    setError('');
    onClose();
  }, [onClose]);

  if (!application) return null;

  const content = (
    <Box sx={contentSx}>
      <Typography
        variant="bodyMedium"
        sx={descriptionSx}
      >
        {t(
          'apps.requestAccessModal.description',
          'Access to this feature requires approval. Please provide your project details and describe your use case.',
        )}
      </Typography>

      <Box sx={fieldWrapperSx}>
        <InputBase
          label={t('apps.requestAccessModal.reasonLabel', 'Reason *')}
          value={reason}
          error={Boolean(error)}
          helperText={error}
          placeholder={t(
            'apps.requestAccessModal.reasonPlaceholder',
            'Describe why you need access to this application...',
          )}
          onChange={handleReasonChange}
          expand={{ minRows: 6, maxRows: 6 }}
        />
      </Box>
    </Box>
  );

  const actions = (
    <Box sx={actionsSx}>
      <BaseBtn
        variant="secondary"
        disabled={isSubmitting}
        onClick={handleClose}
      >
        {t('apps.requestAccessModal.cancel', 'Cancel')}
      </BaseBtn>
      <BaseBtn
        variant="contained"
        disabled={isSubmitting || !reason.trim()}
        onClick={handleSubmit}
      >
        {t('apps.requestAccessModal.send', 'Send Request')}
      </BaseBtn>
    </Box>
  );

  return (
    <BaseModal
      open={open}
      variant="simple"
      title={t('apps.requestAccessModal.title', 'Request Access')}
      content={content}
      onClose={handleClose}
      actions={{ node: actions }}
    />
  );
}
