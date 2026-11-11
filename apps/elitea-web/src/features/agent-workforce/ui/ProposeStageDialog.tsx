/**
 * Propose a ladder stage for the agent: the stage, the rationale, the exact
 * narrowing of a demotion, and the impact preview for suspend, retire and
 * delete.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BaseModal } from '@/shared/ui/BaseModal';

import { EMPTY_DEMOTION, STAGES, narrowingOf, stageLabel, workforceRefusal, type DemotionForm, type Stage } from '../lib/workforce';
import { useImpact, useWorkforceActions } from '../model/useAgentWorkforce';
import { ImpactPreview } from './ImpactPreview';

export interface ProposeStageDialogProps {
  readonly open: boolean;
  readonly projectId: string;
  readonly applicationId: number;
  readonly onClose: () => void;
}

export function ProposeStageDialog({ open, projectId, applicationId, onClose }: ProposeStageDialogProps): ReactNode {
  const [stage, setStage] = useState<Stage>('watch');
  const [rationale, setRationale] = useState('');
  const [demotion, setDemotion] = useState<DemotionForm>(EMPTY_DEMOTION);
  const [error, setError] = useState<string | undefined>(undefined);
  const actions = useWorkforceActions(projectId, applicationId);
  const severe = stage === 'suspend' || stage === 'retire' || stage === 'delete';
  const impact = useImpact(projectId, applicationId, open && severe);
  const narrowing = stage === 'demote' ? narrowingOf(demotion) : undefined;
  const ready = rationale.trim() !== '' && (stage !== 'demote' || narrowing !== undefined);

  const flag = (key: keyof DemotionForm, label: string): ReactNode => (
    <FormControlLabel
      control={
        <BaseCheckbox
          checked={demotion[key] === true}
          onChange={(event) => setDemotion({ ...demotion, [key]: event.target.checked })}
          data-testid={`agent-workforce-narrow-${key}`}
        />
      }
      label={label}
    />
  );

  const submit = (): void => {
    setError(undefined);
    actions.propose.mutate(
      { stage, rationale: rationale.trim(), ...(narrowing !== undefined ? { narrowing } : {}) },
      {
        onSuccess: () => {
          setRationale('');
          setDemotion(EMPTY_DEMOTION);
          onClose();
        },
        onError: (failure) => setError(workforceRefusal(failure, t('features.agentWorkforce.propose.failed', 'The proposal could not be made.')).message),
      },
    );
  };

  return (
    <BaseModal
      open={open}
      data-testid="agent-workforce-propose-dialog"
      title={t('features.agentWorkforce.propose.title', 'Propose a stage')}
      onClose={onClose}
      {...(ready ? { onConfirm: submit } : {})}
      actions={{ confirmText: t('features.agentWorkforce.propose.submit', 'Propose'), confirming: actions.propose.isPending }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem', minWidth: '28rem' }}>
          <TextField
            select
            size="small"
            label={t('features.agentWorkforce.propose.stage', 'Stage')}
            value={stage}
            onChange={(event) => setStage(event.target.value as Stage)}
            slotProps={{ htmlInput: { 'data-testid': 'agent-workforce-propose-stage' } }}
          >
            {STAGES.map((value) => (
              <MenuItem key={value} value={value}>
                {stageLabel(value)}
              </MenuItem>
            ))}
          </TextField>
          <TextField
            multiline
            minRows={3}
            size="small"
            label={t('features.agentWorkforce.propose.rationale', 'Rationale and evidence')}
            value={rationale}
            onChange={(event) => setRationale(event.target.value)}
            slotProps={{ htmlInput: { maxLength: 4000, 'data-testid': 'agent-workforce-propose-rationale' } }}
          />
          {stage === 'demote' && (
            <Box data-testid="agent-workforce-narrowing" sx={{ display: 'flex', flexDirection: 'column' }}>
              <Typography variant="labelMedium">{t('features.agentWorkforce.narrowing.title', 'Take away exactly')}</Typography>
              <TextField
                size="small"
                label={t('features.agentWorkforce.narrowing.toolkits', 'Toolkit ids to detach (comma-separated)')}
                value={demotion.removeToolkitIds}
                onChange={(event) => setDemotion({ ...demotion, removeToolkitIds: event.target.value })}
              />
              {flag('memoryReview', t('features.agentWorkforce.narrowing.memoryReview', 'Memory writes need review'))}
              {flag('proactivityOff', t('features.agentWorkforce.narrowing.proactivityOff', 'Turn the heartbeat off'))}
              {flag('removeLead', t('features.agentWorkforce.narrowing.removeLead', 'Stop leading conversations'))}
              {flag('removeProposeAgents', t('features.agentWorkforce.narrowing.removePropose', 'Remove: may propose agents'))}
              {flag('removeReviewAgents', t('features.agentWorkforce.narrowing.removeReview', 'Remove: reviews agents (HR)'))}
            </Box>
          )}
          {severe && <ImpactPreview impact={impact.data} loading={impact.isLoading} />}
          {error !== undefined && (
            <Typography variant="bodySmall" color="error.main" data-testid="agent-workforce-propose-error">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}
