import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentChangeRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useRequestChange } from '../api/governanceApi';
import { isValidPublishVersionName, type PublishCategory } from '../lib/publishValidation';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';
import { changeRequestStateLabel, tierLabel } from './governanceLabels';
import { PublishPreparationStep } from './PublishPreparationStep';

/**
 * Request publish (ADR-0040 decision 3, #1497).
 *
 * An agent whose risk tier is medium or above is not published directly: the
 * server answers a direct publish with 409 `change_request_required`. This
 * dialog is what "Publish" opens for such an agent instead. It collects what
 * the publish would have (version name, category, the terms) plus a summary
 * for the approvers, creates a `release` change request for the open version
 * and submits it. Submitting starts the release gate; the final required
 * approval publishes the candidate. The dialog ends on the request's state, so
 * the author sees that nothing is published yet.
 */
export interface RequestPublishDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
  /** The agent's tier, when the registry read has answered. */
  readonly riskTier: string | undefined;
  readonly onClose: () => void;
}

export function RequestPublishDialog(props: RequestPublishDialogProps): ReactNode {
  // Remounted per opening, so a second request never starts from the first one's fields.
  return props.open ? <RequestPublishBody {...props} /> : null;
}

function RequestPublishBody({
  projectId,
  applicationId,
  versionId,
  riskTier,
  onClose,
}: RequestPublishDialogProps): ReactNode {
  const [versionName, setVersionName] = useState('');
  const [category, setCategory] = useState<PublishCategory | ''>('');
  const [agreed, setAgreed] = useState(false);
  const [summary, setSummary] = useState('');
  const request = useRequestChange(projectId, applicationId);
  const nameValid = isValidPublishVersionName(versionName);
  const canSubmit = isRequestReady({ nameValid, category, agreed, summary, versionId });

  const submit = (): void => {
    if (!canSubmit || request.isPending) return;
    request.mutate({ kind: 'release', version_id: versionId, version_name: versionName, category, summary: summary.trim() });
  };

  return (
    <Dialog
      open
      onClose={onClose}
      maxWidth="sm"
      fullWidth
      data-testid="request-publish-dialog"
    >
      <DialogTitle>{t('features.agentLifecycle.requestPublish.title', 'Request publish')}</DialogTitle>
      <DialogContent>
        {request.data === undefined ? (
          <Box sx={contentSx}>
            <Typography variant="bodyMedium">
              {t(
                'features.agentLifecycle.requestPublish.intro',
                'This agent is {{tier}} risk, so publishing needs an approved change request. Submitting starts the release gate; the version is published when the last required approval is given.',
                { tier: tierLabel(riskTier ?? 'medium') },
              )}
            </Typography>
            <TextField
              label={t('features.agentLifecycle.requestPublish.summary', 'Summary for the approvers')}
              value={summary}
              onChange={(event) => setSummary(event.target.value)}
              multiline
              minRows={3}
              slotProps={{ htmlInput: { maxLength: 2000, 'data-testid': 'request-publish-summary' } }}
              fullWidth
            />
            <PublishPreparationStep
              versionName={versionName}
              onVersionNameChange={setVersionName}
              nameValid={nameValid}
              category={category}
              onCategoryChange={setCategory}
              agreed={agreed}
              onAgreedChange={setAgreed}
            />
            {request.error !== null && (
              <Alert
                severity="error"
                data-testid="request-publish-error"
              >
                {lifecycleErrorMessage(request.error)}
              </Alert>
            )}
          </Box>
        ) : (
          <RequestOutcome changeRequest={request.data} />
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>
          {request.data === undefined
            ? t('features.agentLifecycle.requestPublish.cancel', 'Cancel')
            : t('features.agentLifecycle.requestPublish.close', 'Close')}
        </Button>
        {request.data === undefined && (
          <Button
            variant="contained"
            disabled={!canSubmit || request.isPending}
            onClick={submit}
            data-testid="request-publish-submit"
          >
            {t('features.agentLifecycle.requestPublish.submit', 'Submit for approval')}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}

function isRequestReady(fields: {
  readonly nameValid: boolean;
  readonly category: string;
  readonly agreed: boolean;
  readonly summary: string;
  readonly versionId: number | undefined;
}): boolean {
  return (
    fields.nameValid &&
    fields.category !== '' &&
    fields.agreed &&
    fields.summary.trim() !== '' &&
    fields.versionId !== undefined
  );
}

function RequestOutcome({ changeRequest }: { readonly changeRequest: AgentChangeRequest }): ReactNode {
  return (
    <Box
      sx={contentSx}
      data-testid="request-publish-outcome"
    >
      <Alert severity="success">
        {t(
          'features.agentLifecycle.requestPublish.submitted',
          'Change request #{{id}} is {{state}}. It needs {{approvals}} approval(s) before the version is published.',
          {
            id: changeRequest.id,
            state: changeRequestStateLabel(changeRequest.state),
            approvals: changeRequest.required_approvals,
          },
        )}
      </Alert>
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {t(
          'features.agentLifecycle.requestPublish.followUp',
          'Follow it, and the release gate, under ⋮ › Governance › Change requests.',
        )}
      </Typography>
    </Box>
  );
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem', paddingTop: '0.5rem' };
