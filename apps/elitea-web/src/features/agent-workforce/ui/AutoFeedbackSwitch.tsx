/**
 * The agents.workforce.auto_feedback switch of the policy page (ADR-0051
 * decision 6): a workforce manager's drafted report on a dismissed or demoted
 * hired agent publishes when the stage is applied, recorded under the person
 * who turned the switch on.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';

interface AutoFeedbackSwitchProps {
  readonly checked: boolean;
  readonly disabled: boolean;
  readonly onChange: (checked: boolean) => void;
}

export function AutoFeedbackSwitch({ checked, disabled, onChange }: AutoFeedbackSwitchProps): ReactNode {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
      <FormControlLabel
        control={
          <BaseSwitch
            checked={checked}
            disabled={disabled}
            onChange={(event) => onChange(event.target.checked)}
            aria-label={t('features.agentWorkforce.policy.autoFeedback', 'Publish workforce reports automatically')}
            data-testid="agent-workforce-policy-auto-feedback"
          />
        }
        label={t('features.agentWorkforce.policy.autoFeedback', 'Publish workforce reports automatically')}
      />
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'features.agentWorkforce.policy.autoFeedbackHelp',
          'When a dismissal or demotion of an installed or forked agent is applied, the report a workforce manager drafted is published to the listing version without waiting for approval. It is recorded under you as the person who set this policy. Off by default.',
        )}
      </Typography>
    </Box>
  );
}
