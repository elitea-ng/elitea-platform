/**
 * One agent's context meter in its participant row (ADR-0050 decision 9b2,
 * issue 1610): how full that agent's own context is, read from the
 * conversation's context status (`agent_contexts`, see
 * `../lib/agentContexts.ts`). It shares the panel's query, so a row adds no
 * request. It renders nothing until the agent has answered.
 *
 * The participant list is a feature and may not import a widget; the page
 * fills its `renderAgentContext` slot with this.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { contextManagementApi } from '@/entities/conversation';
import { t } from '@/shared/i18n';
import { visuallyHidden } from '@/shared/lib/visuallyHidden';

import { readAgentContexts } from '../lib/agentContexts';

/** Warn at the worker's 90% compaction trigger, like the panel. */
const HIGH_PERCENTAGE = 90;

/** @public */
export interface AgentContextMeterProps {
  readonly projectId: string | number | undefined;
  readonly conversationId: string | number | undefined;
  /** The agent's chat participant id (the participant row's `id`). */
  readonly participantId: number | undefined;
}

export function AgentContextMeter({ projectId, conversationId, participantId }: AgentContextMeterProps): ReactNode {
  const enabled = projectId !== undefined && projectId !== '' && conversationId !== undefined && conversationId !== '' &&
    participantId !== undefined;
  const { data } = contextManagementApi.useGetStatus(
    { projectId: projectId ?? '', conversationId: conversationId ?? '' },
    { enabled },
  );
  if (!enabled) return null;
  const meter = readAgentContexts(data).get(participantId);
  if (!meter) return null;
  const high = meter.percentage >= HIGH_PERCENTAGE;
  const title = meter.idleCompacted
    ? t('widgets.contextBudget.agent.idleCompacted', 'Context {{tokens}} tokens. Summarised while idle: the next answer starts from the summary.', { tokens: meter.tokensDisplay })
    : t('widgets.contextBudget.agent.tokens', 'Context {{tokens}} tokens', { tokens: meter.tokensDisplay });
  return (
    <Tooltip title={title} placement="left">
      <Box
        data-testid={`agent-context-meter-${String(participantId)}`}
        data-percentage={meter.percentage}
        data-idle-compacted={meter.idleCompacted ? 'true' : 'false'}
        sx={{ display: 'flex', alignItems: 'center', gap: '0.375rem', flexShrink: 0 }}
      >
        <Box aria-hidden sx={{ width: '2rem', height: '0.25rem', position: 'relative' }}>
          <Box
            sx={(theme: Theme) => ({
              position: 'absolute',
              inset: 0,
              backgroundColor: theme.vars.palette.border.lines,
              borderRadius: theme.vars.shape.radiusPill,
            })}
          />
          <Box
            sx={(theme: Theme) => ({
              position: 'absolute',
              left: 0,
              top: 0,
              bottom: 0,
              width: `${meter.percentage}%`,
              backgroundColor: high ? theme.vars.palette.warning.yellow : theme.vars.palette.success.main,
              borderRadius: theme.vars.shape.radiusPill,
            })}
          />
        </Box>
        <Typography variant="bodySmall" color="text.secondary">
          {t('widgets.contextBudget.agent.percentage', '{{percentage}}%', { percentage: meter.percentage })}
          <Box component="span" sx={visuallyHidden}>
            {` ${title}`}
          </Box>
        </Typography>
      </Box>
    </Tooltip>
  );
}
