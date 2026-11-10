/**
 * The two dialogs of a proposal row: approve (impact preview first, and the
 * nesting confirmation a retirement of a nested agent needs) and the owner's
 * comment or appeal.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { AgentWorkforceAgent, AgentWorkforceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BaseModal } from '@/shared/ui/BaseModal';

import { stageLabel, workforceRefusal } from '../lib/workforce';
import { useImpact, useWorkforceActions } from '../model/useAgentWorkforce';
import { ImpactPreview } from './ImpactPreview';

export type OwnerTextAction = 'comment' | 'appeal';

interface DialogProps {
  readonly projectId: string;
  readonly agent: AgentWorkforceAgent;
  readonly proposal: AgentWorkforceProposal;
  readonly onClose: () => void;
}

function isSevere(stage: string): boolean {
  return stage === 'suspend' || stage === 'retire' || stage === 'delete';
}

export function ApproveDialog({ open, projectId, agent, proposal, onClose }: DialogProps & { readonly open: boolean }): ReactNode {
  const actions = useWorkforceActions(projectId, agent.application_id);
  const severe = isSevere(proposal.stage);
  const impact = useImpact(projectId, agent.application_id, open && severe);
  const [confirmNested, setConfirmNested] = useState(false);
  const [needsConfirm, setNeedsConfirm] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);
  const nested = needsConfirm || (impact.data?.nested_by.length ?? 0) > 0;

  const approve = (): void => {
    setError(undefined);
    actions.decide.mutate(
      { proposalId: proposal.id, decision: 'approve', note: '', confirmNested },
      {
        onSuccess: onClose,
        onError: (failure) => {
          const refusal = workforceRefusal(failure, t('features.agentWorkforce.decision.failed', 'That did not go through.'));
          if (refusal.code === 'workforce_confirm_nested') setNeedsConfirm(true);
          setError(refusal.message);
        },
      },
    );
  };

  return (
    <BaseModal
      open={open}
      data-testid="agent-workforce-approve-dialog"
      title={t('features.agentWorkforce.approve.title', 'Approve: {{stage}}', { stage: stageLabel(proposal.stage) })}
      onClose={onClose}
      onConfirm={approve}
      actions={{ confirmText: t('features.agentWorkforce.action.approve', 'Approve'), alarm: severe, confirming: actions.decide.isPending }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', minWidth: '24rem' }}>
          {severe && <ImpactPreview impact={impact.data} loading={impact.isLoading} />}
          {nested && (
            <FormControlLabel
              control={
                <BaseCheckbox
                  checked={confirmNested}
                  onChange={(event) => setConfirmNested(event.target.checked)}
                  data-testid="agent-workforce-confirm-nested"
                />
              }
              label={t('features.agentWorkforce.approve.confirmNested', 'I understand the agents that nest it will fail')}
            />
          )}
          {error !== undefined && (
            <Typography variant="bodySmall" color="error.main" data-testid="agent-workforce-approve-error">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}

export function OwnerTextDialog({ projectId, agent, proposal, action, onClose }: DialogProps & { readonly action: OwnerTextAction | undefined }): ReactNode {
  const actions = useWorkforceActions(projectId, agent.application_id);
  const [text, setText] = useState('');
  const [error, setError] = useState<string | undefined>(undefined);
  const submit = (): void => {
    if (action === undefined) return;
    actions.ownerText.mutate(
      { proposalId: proposal.id, kind: action, text: text.trim() },
      {
        onSuccess: onClose,
        onError: (failure) => setError(workforceRefusal(failure, t('features.agentWorkforce.decision.failed', 'That did not go through.')).message),
      },
    );
  };
  const title =
    action === 'appeal'
      ? t('features.agentWorkforce.appeal.title', 'Appeal this decision')
      : t('features.agentWorkforce.comment.title', 'Comment as the owner');
  return (
    <BaseModal
      open={action !== undefined}
      data-testid="agent-workforce-text-dialog"
      title={title}
      onClose={onClose}
      {...(text.trim() === '' ? {} : { onConfirm: submit })}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', minWidth: '24rem' }}>
          <TextField
            multiline
            minRows={3}
            fullWidth
            size="small"
            value={text}
            onChange={(event) => setText(event.target.value)}
            slotProps={{ htmlInput: { maxLength: 4000, 'data-testid': 'agent-workforce-text' } }}
          />
          {error !== undefined && (
            <Typography variant="bodySmall" color="error.main">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}
