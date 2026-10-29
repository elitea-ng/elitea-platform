import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentChangeRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useChangeRequestAction, type ChangeRequestActionOutcome } from '../api/governanceApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';

/**
 * The actions an open change request allows: approve (with a note), reject
 * and withdraw (with a reason), and override a failed release gate.
 *
 * The buttons are offered by STATE only. Whether this caller may approve
 * (not the proposer, the governance role at high and critical) is the
 * server's decision, and its refusal is shown as a sentence
 * (`self_approval_refused`, `approver_role_required`, ...) rather than
 * guessed at here.
 *
 * The override reason is capped at 200 characters: the audit event's name
 * holds 256, and the server refuses a longer one.
 */
const OVERRIDE_REASON_MAX = 200;

export interface ChangeRequestActionsProps {
  readonly projectId: string;
  readonly applicationId: number;
  readonly request: AgentChangeRequest;
}

const OPEN_STATES = new Set(['draft', 'submitted']);

export function ChangeRequestActions({ projectId, applicationId, request }: ChangeRequestActionsProps): ReactNode {
  const [note, setNote] = useState('');
  const [overrideReason, setOverrideReason] = useState('');
  const [outcome, setOutcome] = useState<ChangeRequestActionOutcome>();
  const action = useChangeRequestAction(projectId, applicationId);
  if (!OPEN_STATES.has(request.state)) {
    return outcome === undefined ? null : <OutcomeNotice outcome={outcome} />;
  }
  const run = (next: Parameters<typeof action.mutate>[0]): void =>
    action.mutate(next, {
      onSuccess: (result) => {
        setNote('');
        setOverrideReason('');
        setOutcome(result);
      },
    });
  const canOverride = request.gate?.state === 'failed' && !request.gate.overridden;

  return (
    <Box
      sx={stackSx}
      data-testid="change-request-actions"
    >
      <TextField
        label={t('features.agentLifecycle.changeRequest.note', 'Note or reason (optional)')}
        value={note}
        onChange={(event) => setNote(event.target.value)}
        size="small"
        slotProps={{ htmlInput: { maxLength: 500, 'data-testid': 'change-request-note' } }}
      />
      <Box sx={rowSx}>
        {request.state === 'submitted' && (
          <Button
            variant="contained"
            size="small"
            disabled={action.isPending}
            onClick={() => run({ kind: 'approve', id: request.id, note })}
            data-testid="change-request-approve"
          >
            {t('features.agentLifecycle.changeRequest.approve', 'Approve')}
          </Button>
        )}
        {request.state === 'submitted' && (
          <Button
            size="small"
            color="error"
            disabled={action.isPending}
            onClick={() => run({ kind: 'reject', id: request.id, reason: note })}
            data-testid="change-request-reject"
          >
            {t('features.agentLifecycle.changeRequest.reject', 'Reject')}
          </Button>
        )}
        <Button
          size="small"
          disabled={action.isPending}
          onClick={() => run({ kind: 'withdraw', id: request.id, reason: note })}
          data-testid="change-request-withdraw"
        >
          {t('features.agentLifecycle.changeRequest.withdraw', 'Withdraw')}
        </Button>
      </Box>
      {canOverride && (
        <Box sx={rowSx}>
          <TextField
            label={t('features.agentLifecycle.changeRequest.overrideReason', 'Reason to accept the failed gate')}
            value={overrideReason}
            onChange={(event) => setOverrideReason(event.target.value.slice(0, OVERRIDE_REASON_MAX))}
            size="small"
            sx={{ flex: 1 }}
            helperText={`${overrideReason.length}/${OVERRIDE_REASON_MAX}`}
            slotProps={{ htmlInput: { maxLength: OVERRIDE_REASON_MAX, 'data-testid': 'change-request-override-reason' } }}
          />
          <Button
            size="small"
            color="warning"
            disabled={action.isPending || overrideReason.trim() === ''}
            onClick={() => run({ kind: 'override', id: request.id, reason: overrideReason.trim() })}
            data-testid="change-request-override"
          >
            {t('features.agentLifecycle.changeRequest.override', 'Override gate')}
          </Button>
        </Box>
      )}
      {action.error !== null && (
        <Alert
          severity="error"
          data-testid="change-request-error"
        >
          {lifecycleErrorMessage(action.error)}
        </Alert>
      )}
      {outcome !== undefined && <OutcomeNotice outcome={outcome} />}
    </Box>
  );
}

function OutcomeNotice({ outcome }: { readonly outcome: ChangeRequestActionOutcome }): ReactNode {
  return (
    <Alert
      severity="success"
      data-testid="change-request-outcome"
    >
      {outcome.published === undefined
        ? t('features.agentLifecycle.changeRequest.done', 'Saved. The change request is now {{state}}.', {
            state: outcome.changeRequest.state,
          })
        : t(
            'features.agentLifecycle.changeRequest.publishedNotice',
            'Approved. That was the last required approval, so version {{version}} is published.',
            { version: outcome.published.version_name },
          )}
    </Alert>
  );
}

const stackSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '0.5rem' };
