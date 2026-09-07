/**
 * One decision, one reason, every type the filters are showing.
 *
 * ## Why this exists
 *
 * "Disable every test-management toolkit for this client" is fifty dialogs
 * otherwise, and an operator doing it fifty times will stop halfway and leave
 * the catalogue in a state nobody meant. The server's bulk route exists for it.
 *
 * ## It acts on the FILTERED rows, and it says how many
 *
 * The set is whatever the page is showing, so what the operator sees is what
 * they change. The count and the first few names are on screen before the
 * confirm, because a bulk control whose subject is invisible is a control an
 * operator cannot check before pressing it.
 *
 * ## The server can land some and refuse others
 *
 * The bulk route is not atomic and reports which types landed. It answers 400
 * when NOTHING landed, so a bulk apply that changed nothing surfaces here as a
 * failure rather than as a silent success.
 */
import { useCallback, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import {
  toolkitTypeFailureReason,
  useBulkToolkitTypePolicy,
  type ToolkitTypeAvailability,
} from './api/adminToolkitTypesApi';

export interface ToolkitTypeBulkDialogProps {
  readonly open: boolean;
  /** The type keys the page is currently showing. */
  readonly types: readonly string[];
  readonly onClose: () => void;
}

export function ToolkitTypeBulkDialog({ open, types, onClose }: ToolkitTypeBulkDialogProps) {
  const bulk = useBulkToolkitTypePolicy();

  const [availability, setAvailability] = useState<ToolkitTypeAvailability>('disabled');
  const [reason, setReason] = useState('');
  const [failure, setFailure] = useState<string | null>(null);

  const trimmed = reason.trim();
  const reverting = availability === 'default';
  const submittable = types.length > 0 && (reverting || trimmed !== '') && !bulk.isPending;

  const close = useCallback(() => {
    setReason('');
    setFailure(null);
    onClose();
  }, [onClose]);

  const submit = useCallback(async () => {
    if (!submittable) return;
    setFailure(null);
    try {
      await bulk.mutateAsync({ types, availability, reason: trimmed });
      setReason('');
      onClose();
    } catch (error) {
      setFailure(
        toolkitTypeFailureReason(error) ??
          t('pages.admin.toolkitTypes.error.bulk', 'The toolkit types could not be changed.'),
      );
    }
  }, [availability, bulk, onClose, submittable, trimmed, types]);

  return (
    <Dialog open={open} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>{t('pages.admin.toolkitTypes.bulk.title', 'Apply to the listed types')}</DialogTitle>
      <DialogContent>
        <DialogContentText sx={{ marginBottom: '1rem' }}>
          {t(
            'pages.admin.toolkitTypes.bulk.body',
            'The decision is applied to every type the filters are showing. Types that cannot be changed are reported and the rest still land.',
          )}
        </DialogContentText>
        <DialogContentText sx={{ marginBottom: '1rem' }} data-testid="admin-toolkit-types-bulk-subject">
          {t('pages.admin.toolkitTypes.bulk.subject', '{{count}} types: {{names}}', {
            count: types.length,
            names: types.slice(0, 8).join(', '),
          })}
        </DialogContentText>
        <TextField
          select
          fullWidth
          label={t('pages.admin.toolkitTypes.bulk.availability', 'Availability')}
          value={availability}
          onChange={(event) => setAvailability(event.target.value as ToolkitTypeAvailability)}
        >
          <MenuItem value="default">
            {t('pages.admin.toolkitTypes.availability.default', 'Default')}
          </MenuItem>
          <MenuItem value="enabled">
            {t('pages.admin.toolkitTypes.availability.enabled', 'Enabled')}
          </MenuItem>
          <MenuItem value="disabled">
            {t('pages.admin.toolkitTypes.availability.disabled', 'Disabled')}
          </MenuItem>
          <MenuItem value="restricted">
            {t('pages.admin.toolkitTypes.availability.restricted', 'Restricted')}
          </MenuItem>
        </TextField>
        <TextField
          fullWidth
          required={!reverting}
          multiline
          minRows={2}
          sx={{ marginTop: '1rem' }}
          label={t('pages.admin.toolkitTypes.reason.label', 'Reason')}
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        {failure === null ? null : (
          <Alert severity="error" sx={{ marginTop: '1rem' }} data-testid="admin-toolkit-types-bulk-error">
            {failure}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={close} disabled={bulk.isPending}>
          {t('pages.admin.toolkitTypes.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          disabled={!submittable}
          onClick={() => {
            void submit();
          }}
        >
          {t('pages.admin.toolkitTypes.action.applyBulk', 'Apply')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
