/**
 * The activate / deactivate decision dialog for the admin Service Descriptors
 * page (migration 0109, ADR-0023 S3).
 *
 * ## Why a dialog and not a `window.confirm`
 *
 * The reference's per-row delete sits behind a `window.confirm`, and that is
 * exactly what will not do here. An activation RECORDS AN OPERATOR'S SENTENCE
 * on the revision row — the server requires a non-empty `reason` and refuses a
 * request without one, because a decision to put an external provider in force
 * with no recorded reason cannot be reviewed later. A confirm dialog cannot
 * collect a sentence, so it would have to be invented client-side, and an
 * invented reason in an audit trail is worse than none.
 *
 * ## Why the dialog stays open on a failure
 *
 * The 422 is the interesting outcome: it means the provider republished between
 * the review and the click, so the manifest the operator read is not the one
 * they were about to activate. That is the one message they most need, and it
 * arrives with the reason they typed still in the box, ready to resubmit against
 * a re-read row. Closing the dialog and refreshing the list would replace it
 * with a row that silently did not change.
 *
 * ## Why it is a separate component
 *
 * Not tidiness: the page's own function tripped the complexity gate with the
 * dialog inline, and the two really are separate jobs — the page decides WHICH
 * row is under decision, this decides WHAT is sent for it.
 */
import { useCallback, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import {
  descriptorFailureReason,
  useActivateServiceDescriptor,
  useDeactivateServiceDescriptor,
  type AdminServiceDescriptor,
} from './api/adminServiceDescriptorsApi';

/**
 * The row an operator is deciding about, and which verb they chose. ONE piece
 * of state for both dialogs: two independent `open` booleans is how a page ends
 * up rendering both at once.
 */
export interface AdmissionDecision {
  readonly verb: 'activate' | 'deactivate';
  readonly descriptor: AdminServiceDescriptor;
}

export interface ServiceDescriptorDecisionDialogProps {
  /** `null` closes it. */
  readonly decision: AdmissionDecision | null;
  readonly onClose: () => void;
}

/** The digest an operator reads off the dialog: enough to recognise, not to retype. */
function shortDigest(descriptor: AdminServiceDescriptor): string {
  const digest = descriptor.published_manifest_digest;
  return digest === null || digest === undefined ? '' : ` · ${digest.slice(0, 16)}`;
}

export function ServiceDescriptorDecisionDialog({
  decision,
  onClose,
}: ServiceDescriptorDecisionDialogProps) {
  const activate = useActivateServiceDescriptor();
  const deactivate = useDeactivateServiceDescriptor();

  const [reason, setReason] = useState('');
  const [failure, setFailure] = useState<string | null>(null);

  const trimmed = reason.trim();
  const submitting = activate.isPending || deactivate.isPending;
  const deactivating = decision?.verb === 'deactivate';

  const close = useCallback(() => {
    setReason('');
    setFailure(null);
    onClose();
  }, [onClose]);

  const submit = useCallback(async () => {
    if (decision === null || trimmed === '') return;
    setFailure(null);
    try {
      if (decision.verb === 'deactivate') {
        await deactivate.mutateAsync({
          projectId: decision.descriptor.project_id,
          providerName: decision.descriptor.provider_name,
          reason: trimmed,
        });
      } else {
        // The digest is the ROW's, not one re-read at click time: the request
        // asserts what the operator was looking at when they decided. A digest
        // fetched now would agree with whatever the provider had just
        // published, which is the case the compare-and-swap exists to catch.
        await activate.mutateAsync({
          projectId: decision.descriptor.project_id,
          providerName: decision.descriptor.provider_name,
          expectedDigest: decision.descriptor.published_manifest_digest ?? '',
          reason: trimmed,
        });
      }
      setReason('');
      onClose();
    } catch (error) {
      setFailure(
        descriptorFailureReason(error) ??
          t(
            'pages.admin.serviceDescriptors.error.write',
            'The provider admission could not be changed.',
          ),
      );
    }
  }, [activate, deactivate, decision, onClose, trimmed]);

  const confirmLabel = deactivating
    ? t('pages.admin.serviceDescriptors.action.deactivate', 'Deactivate')
    : t('pages.admin.serviceDescriptors.action.activate', 'Activate');

  return (
    <Dialog open={decision !== null} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>
        {deactivating
          ? t('pages.admin.serviceDescriptors.deactivate.title', 'Deactivate provider')
          : t('pages.admin.serviceDescriptors.activate.title', 'Activate provider')}
      </DialogTitle>
      <DialogContent>
        <DialogContentText sx={{ marginBottom: '1rem' }}>
          {deactivating
            ? t(
                'pages.admin.serviceDescriptors.deactivate.body',
                'The provider stops being in force. Its recorded revision and the policy overlay it ran under are kept.',
              )
            : t(
                'pages.admin.serviceDescriptors.activate.body',
                'The reviewed manifest is put in force under a policy overlay. If the provider has republished since it was reviewed, the request is refused.',
              )}
        </DialogContentText>
        {decision === null ? null : (
          <Typography variant="bodyMedium" component="p" color="text.secondary" sx={{ marginBottom: '1rem' }}>
            {decision.descriptor.provider_name}
            {shortDigest(decision.descriptor)}
          </Typography>
        )}
        {/* REQUIRED here and at the server. The submit button is disabled until
            there is one, so the refusal is visible before the request rather
            than as a 400 afterwards. */}
        <TextField
          fullWidth
          required
          multiline
          minRows={2}
          label={t('pages.admin.serviceDescriptors.reason.label', 'Reason')}
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        {failure === null ? null : (
          <Alert
            severity="error"
            sx={{ marginTop: '1rem' }}
            data-testid="admin-service-descriptors-write-error"
          >
            {failure}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={close} disabled={submitting}>
          {t('pages.admin.serviceDescriptors.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          disabled={trimmed === '' || submitting}
          onClick={() => {
            void submit();
          }}
        >
          {confirmLabel}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
