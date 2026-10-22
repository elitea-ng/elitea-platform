/**
 * The reject-with-a-reason dialog for the admin App Requests page (unit A14).
 *
 * Reference:
 * `frontends/admin_ui/frontend/src/pages/AppRequestsPage/RejectRequestDialog.jsx`
 * (read-only). Same job, rewritten on this app's MUI 9 and with the request's
 * own details on screen — the reference dialog shows only the words "Reject
 * Request", so an operator working a queue confirms a rejection without seeing
 * which row they are rejecting.
 *
 * The reason is REQUIRED, and that is not merely a form nicety here: the server
 * refuses a rejection with an empty comment (the legacy validator does not fire
 * when the key is absent, which is how a null-reason rejection got through
 * there), and the comment is what the requester is told. A dialog that let the
 * field through empty would be sending a request it knows will be refused.
 */
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import { memo, useCallback, useEffect, useState } from 'react';

import { t } from '@/shared/i18n';

import type { AppRequestRow } from './api/adminAppRequestsApi';

export interface RejectAppRequestDialogProps {
  /** The request being rejected, or `null` when the dialog is closed. */
  request: AppRequestRow | null;
  onCancel: () => void;
  /** Absent ⇒ this user may not decide, so the dialog cannot be confirmed. */
  onConfirm: ((comment: string) => void) | undefined;
}

export const RejectAppRequestDialog = memo(function RejectAppRequestDialog({
  request,
  onCancel,
  onConfirm,
}: RejectAppRequestDialogProps) {
  const [comment, setComment] = useState('');
  const [touched, setTouched] = useState(false);

  // Cleared when the dialog opens on a different request. The reference keeps
  // one component mounted and resets in its handlers, which leaves the previous
  // row's reason in the box if the dialog is dismissed by any other route.
  useEffect(() => {
    if (request !== null) {
      setComment('');
      setTouched(false);
    }
  }, [request]);

  const invalid = comment.trim() === '';

  const handleConfirm = useCallback(() => {
    setTouched(true);
    if (invalid || onConfirm === undefined) return;
    onConfirm(comment);
  }, [comment, invalid, onConfirm]);

  return (
    <Dialog open={request !== null} onClose={onCancel} maxWidth="sm" fullWidth>
      <DialogTitle>{t('pages.admin.appRequests.reject.title', 'Reject request')}</DialogTitle>
      <DialogContent>
        <DialogContentText sx={{ marginBottom: '1rem' }}>
          {t(
            'pages.admin.appRequests.reject.body',
            'The reason is sent to the person who asked, and is the only thing they are told.',
          )}
        </DialogContentText>
        {request !== null ? (
          <DialogContentText
            data-testid="admin-app-requests-reject-subject"
            sx={{ marginBottom: '1rem', fontWeight: 600 }}
          >
            {request.issue_type} — {request.user_email}
          </DialogContentText>
        ) : null}
        {/*
          No `autoFocus`. The reference dialog has it; `jsx-a11y/no-autofocus`
          forbids it, and MUI's `Dialog` already moves focus into the dialog on
          open, so the field is reachable by one Tab rather than by a jump a
          screen-reader user is not told about.
        */}
        <TextField
          fullWidth
          multiline
          rows={4}
          value={comment}
          onChange={(event) => setComment(event.target.value)}
          error={touched && invalid}
          helperText={
            touched && invalid
              ? t('pages.admin.appRequests.reject.required', 'A reason is required.')
              : ' '
          }
          label={t('pages.admin.appRequests.reject.label', 'Reason')}
          slotProps={{ htmlInput: { 'data-testid': 'admin-app-requests-reject-reason' } }}
        />
      </DialogContent>
      <DialogActions sx={{ paddingX: '1.5rem', paddingBottom: '1rem' }}>
        <Button onClick={onCancel} color="inherit">
          {t('pages.admin.appRequests.reject.cancel', 'Cancel')}
        </Button>
        <Button
          onClick={handleConfirm}
          variant="contained"
          color="error"
          disabled={onConfirm === undefined}
          data-testid="admin-app-requests-reject-confirm"
        >
          {t('pages.admin.appRequests.reject.confirm', 'Reject')}
        </Button>
      </DialogActions>
    </Dialog>
  );
});
