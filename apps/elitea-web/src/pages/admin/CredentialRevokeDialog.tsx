/**
 * The revoke dialog of Admin › Credential inventory (ADR-0044 decision 5,
 * #1501). A dialog, not a `window.confirm`: the reason the administrator
 * types is recorded on the security audit event, and a refusal (the
 * credential was already gone, or is flagged only) stays in the dialog next
 * to the row it was about.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { credentialReviewErrorMessage } from '@/features/agent-lifecycle';
import type { CredentialInventoryCredential } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useRevokeCredential } from './api/adminCredentialReviewApi';

export interface CredentialRevokeDialogProps {
  readonly row: CredentialInventoryCredential | undefined;
  readonly onClose: () => void;
}

export function CredentialRevokeDialog({ row, onClose }: CredentialRevokeDialogProps): ReactNode {
  const [reason, setReason] = useState('');
  const revoke = useRevokeCredential();
  const close = (): void => {
    setReason('');
    revoke.reset();
    onClose();
  };
  const submit = (): void => {
    if (row === undefined) return;
    revoke.mutate(
      {
        kind: row.kind,
        credential_id: row.credential_id,
        reason: reason.trim(),
      },
      { onSuccess: close },
    );
  };
  return (
    <Dialog open={row !== undefined} onClose={close} maxWidth="sm" fullWidth data-testid="credential-revoke-dialog">
      <DialogTitle>{t('pages.admin.credentialInventory.revokeTitle', 'Revoke credential')}</DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium" sx={{ marginBottom: '1rem' }}>
          {t(
            'pages.admin.credentialInventory.revokeBody',
            'Revoke {{name}}? Agents and tools that use it stop working with it. The revocation is audited.',
            { name: row?.name ?? '' },
          )}
        </Typography>
        <TextField
          fullWidth
          size="small"
          label={t('pages.admin.credentialInventory.revokeReason', 'Reason (recorded on the audit event)')}
          value={reason}
          onChange={(event) => setReason(event.target.value.slice(0, 500))}
          slotProps={{
            htmlInput: { 'data-testid': 'credential-revoke-reason' },
          }}
        />
        {revoke.error !== null && (
          <Alert severity="error" sx={{ marginTop: '1rem' }}>
            {credentialReviewErrorMessage(revoke.error)}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>{t('pages.admin.credentialInventory.cancel', 'Cancel')}</Button>
        <Button
          color="error"
          variant="contained"
          disabled={revoke.isPending}
          onClick={submit}
          data-testid="credential-revoke-confirm"
        >
          {t('pages.admin.credentialInventory.revoke', 'Revoke')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
