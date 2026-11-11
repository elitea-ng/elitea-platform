/**
 * The impact preview a ladder decision shows before it is made (ADR-0050
 * decision 5g): what suspending or retiring the agent would touch.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { AgentWorkforceImpact } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

export interface ImpactPreviewProps {
  readonly impact: AgentWorkforceImpact | undefined;
  readonly loading: boolean;
}

export function ImpactPreview({ impact, loading }: ImpactPreviewProps): ReactNode {
  if (loading) {
    return <Typography variant="bodySmall">{t('features.agentWorkforce.impact.loading', 'Reading what this would touch…')}</Typography>;
  }
  if (impact === undefined) return null;
  const lines = [
    t('features.agentWorkforce.impact.conversations', 'In {{count}} conversations, leading {{led}}', {
      count: impact.conversations,
      led: impact.conversations_led,
    }),
    t('features.agentWorkforce.impact.teams', 'In {{count}} saved teams', { count: impact.teams.length }),
    t('features.agentWorkforce.impact.schedules', '{{schedules}} schedules and {{triggers}} triggers', {
      schedules: impact.schedules,
      triggers: impact.triggers,
    }),
    t('features.agentWorkforce.impact.published', '{{count}} published versions', { count: impact.published_versions }),
  ];
  return (
    <Box data-testid="agent-workforce-impact" sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
      <Typography variant="labelMedium">{t('features.agentWorkforce.impact.title', 'What this touches')}</Typography>
      {lines.map((line) => (
        <Typography key={line} variant="bodySmall">
          {line}
        </Typography>
      ))}
      {impact.nested_by.length > 0 && (
        <Typography variant="bodySmall" color="warning.main" data-testid="agent-workforce-impact-nested">
          {t('features.agentWorkforce.impact.nested', 'Nested by {{names}}: they will fail with nested_agent_retired until fixed.', {
            names: impact.nested_by.map((nesting) => nesting.name).join(', '),
          })}
        </Typography>
      )}
      {impact.legal_hold && (
        <Typography variant="bodySmall" color="error.main">
          {t('features.agentWorkforce.impact.legalHold', 'Under legal hold: it cannot be deleted.')}
        </Typography>
      )}
    </Box>
  );
}
