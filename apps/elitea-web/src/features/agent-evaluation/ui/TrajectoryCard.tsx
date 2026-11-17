/**
 * The Trajectory card of a run (issue 2011, step B): the steps and tool calls
 * of each case, the steps-per-case p50 and p95, and the tool-call, error and
 * retry counts. Shown only for a run that executed its cases as agent runs
 * (execution mode `runtime_plane`); a run of one model call per case has no
 * steps to show.
 *
 * Distinct tools, redundant calls, guardrail events and the cases that hit the
 * step limit (issue 2192) sit beside them, with each case's own values in its
 * row. Definitions: docs "Evaluation, Trajectory".
 *
 * A figure that was not recorded reads "not recorded"; it is never printed as 0.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { formatCount, formatMs } from '../lib/insightsFormat';
import type { CaseGuardrail, CaseTrajectory, TrajectoryItem, TrajectorySummary } from '../lib/trajectory';

const cardSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  padding: '0.75rem',
  border: (theme) => `1px solid ${theme.vars.palette.divider}`,
  borderRadius: (theme) => theme.vars.shape.radiusMd,
};
const stripSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '1.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', gap: '1rem', flexWrap: 'wrap', alignItems: 'baseline' };

function Stat({ label, value, testId }: { readonly label: string; readonly value: string; readonly testId: string }): ReactNode {
  return (
    <Box data-testid={testId}>
      <Typography variant="bodySmall" color="text.secondary" component="div">
        {label}
      </Typography>
      <Typography variant="labelMedium" component="div">
        {value}
      </Typography>
    </Box>
  );
}

function notRecorded(): string {
  return t('features.agentEvaluation.insights.notRecorded', 'not recorded');
}

function formatShare(share: number | undefined): string {
  return share === undefined ? '' : new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 0 }).format(share);
}

function distinctToolsValue(summary: TrajectorySummary): string {
  if (summary.distinctToolsPerCase === undefined || summary.distinctToolsInRun === undefined) return notRecorded();
  return t('features.agentEvaluation.trajectory.distinctToolsValue', '{{perCase}} per case, {{inRun}} in all', {
    perCase: formatCount(summary.distinctToolsPerCase),
    inRun: formatCount(summary.distinctToolsInRun),
  });
}

function redundantValue(summary: TrajectorySummary): string {
  if (summary.redundantCalls === undefined) return notRecorded();
  if (summary.redundantShare === undefined) return formatCount(summary.redundantCalls);
  return t('features.agentEvaluation.trajectory.redundantValue', '{{count}} ({{share}} of tool calls)', {
    count: formatCount(summary.redundantCalls),
    share: formatShare(summary.redundantShare),
  });
}

function stepLimitValue(summary: TrajectorySummary): string {
  return t('features.agentEvaluation.trajectory.stepLimitHitValue', '{{count}} of {{total}} case(s)', {
    count: summary.stepLimitCases,
    total: summary.totalCases,
  });
}

function guardrailBreakdown(guardrail: CaseGuardrail): string {
  return t('features.agentEvaluation.trajectory.guardrailBreakdown', '{{blocked}} blocked, {{masked}} masked, {{warned}} warned, {{gated}} held for approval', {
    blocked: guardrail.blocked,
    masked: guardrail.masked,
    warned: guardrail.warned,
    gated: guardrail.gated,
  });
}

function itemLabel(item: TrajectoryItem): string {
  const target = item.name === '' ? item.kind : `${item.kind} ${item.name}`;
  return item.durationMs === undefined ? target : `${target} (${formatMs(item.durationMs)})`;
}

function StepList({ entry }: { readonly entry: CaseTrajectory }): ReactNode {
  return (
    <Box component="ol" sx={{ margin: 0, paddingLeft: '1.25rem' }} data-testid={`trajectory-steps-${entry.caseId}`}>
      {entry.items.map((item) => (
        <li key={item.seq}>
          <Typography variant="bodySmall" component="span" color={item.error === undefined ? 'text.primary' : 'error'}>
            {itemLabel(item)}
            {item.error === undefined ? '' : `: ${item.error}`}
          </Typography>
          {item.output !== undefined && (
            <Typography variant="bodySmall" component="pre" color="text.secondary" sx={{ margin: 0, whiteSpace: 'pre-wrap' }}>
              {item.output}
            </Typography>
          )}
        </li>
      ))}
      {entry.truncated && (
        <Typography variant="bodySmall" color="text.secondary" component="li">
          {t('features.agentEvaluation.trajectory.truncated', 'Only the first steps are listed; the counts cover every step.')}
        </Typography>
      )}
    </Box>
  );
}

function flagText(entry: CaseTrajectory): string | undefined {
  if (entry.timedOut) return t('features.agentEvaluation.trajectory.timedOut', 'ended at the time limit');
  if (entry.stepLimit) return t('features.agentEvaluation.trajectory.stepLimit', 'ended at the step limit');
  return undefined;
}

function CaseRow({ entry }: { readonly entry: CaseTrajectory }): ReactNode {
  const flag = flagText(entry);
  return (
    <Box component="details" data-testid={`trajectory-case-${entry.caseId}`}>
      <Box component="summary" sx={rowSx}>
        <Typography variant="bodyMedium" component="span" sx={{ maxWidth: '24rem' }} noWrap>
          {entry.input ?? entry.caseId}
        </Typography>
        <Typography variant="bodySmall" component="span" color="text.secondary" data-testid={`trajectory-extra-${entry.caseId}`}>
          {t('features.agentEvaluation.trajectory.caseExtra', '{{distinct}} distinct tool(s), {{redundant}} redundant, {{guardrail}} guardrail event(s)', {
            distinct: formatCount(entry.distinctTools),
            redundant: formatCount(entry.redundantCalls),
            guardrail: formatCount(entry.guardrail?.events),
          })}
        </Typography>
        <Typography variant="bodySmall" component="span" data-testid={`trajectory-counts-${entry.caseId}`}>
          {t('features.agentEvaluation.trajectory.counts', '{{steps}} step(s), {{tools}} tool call(s), {{errors}} error(s), {{retries}} retry(ies)', {
            steps: entry.steps,
            tools: entry.toolCalls,
            errors: entry.toolErrors,
            retries: entry.retries,
          })}
        </Typography>
        {flag !== undefined && (
          <Typography variant="bodySmall" color="error" component="span">
            {flag}
          </Typography>
        )}
      </Box>
      {entry.guardrail !== undefined && entry.guardrail.events > 0 && (
        <Typography variant="bodySmall" color="text.secondary" component="div" data-testid={`trajectory-guardrail-${entry.caseId}`}>
          {guardrailBreakdown(entry.guardrail)}
        </Typography>
      )}
      <StepList entry={entry} />
    </Box>
  );
}

export interface TrajectoryCardProps {
  /** `undefined` when no case recorded a trajectory. */
  readonly summary: TrajectorySummary | undefined;
}

export function TrajectoryCard({ summary }: TrajectoryCardProps): ReactNode {
  return (
    <Box sx={cardSx} data-testid="run-trajectory">
      <Typography variant="labelMedium">{t('features.agentEvaluation.trajectory.title', 'Trajectory')}</Typography>
      {summary === undefined ? (
        <Typography variant="bodySmall" color="text.secondary" data-testid="run-trajectory-none">
          {t('features.agentEvaluation.trajectory.none', 'No case recorded its steps.')}
        </Typography>
      ) : (
        <>
          <Box sx={stripSx}>
            <Stat label={t('features.agentEvaluation.trajectory.stepsP50', 'Steps per case p50')} value={formatCount(summary.stepsP50)} testId="run-trajectory-p50" />
            <Stat label={t('features.agentEvaluation.trajectory.stepsP95', 'Steps per case p95')} value={formatCount(summary.stepsP95)} testId="run-trajectory-p95" />
            <Stat label={t('features.agentEvaluation.trajectory.toolCalls', 'Tool calls')} value={formatCount(summary.toolCalls)} testId="run-trajectory-tool-calls" />
            <Stat label={t('features.agentEvaluation.trajectory.toolErrors', 'Tool errors')} value={formatCount(summary.toolErrors)} testId="run-trajectory-tool-errors" />
            <Stat label={t('features.agentEvaluation.trajectory.retries', 'Retries')} value={formatCount(summary.retries)} testId="run-trajectory-retries" />
            <Stat label={t('features.agentEvaluation.trajectory.distinctTools', 'Distinct tools')} value={distinctToolsValue(summary)} testId="run-trajectory-distinct-tools" />
            <Stat label={t('features.agentEvaluation.trajectory.redundantCalls', 'Redundant calls')} value={redundantValue(summary)} testId="run-trajectory-redundant" />
            <Stat label={t('features.agentEvaluation.trajectory.guardrailEvents', 'Guardrail events')} value={formatCount(summary.guardrailEvents)} testId="run-trajectory-guardrail" />
            <Stat label={t('features.agentEvaluation.trajectory.stepLimitHit', 'Step limit hit')} value={stepLimitValue(summary)} testId="run-trajectory-step-limit" />
          </Box>
          {summary.cases.length < summary.totalCases && (
            <Typography variant="bodySmall" color="text.secondary" data-testid="run-trajectory-partial">
              {t('features.agentEvaluation.trajectory.partial', 'Steps were recorded for {{measured}} of {{total}} case(s).', {
                measured: summary.cases.length,
                total: summary.totalCases,
              })}
            </Typography>
          )}
          {summary.guardrailEvents !== undefined && summary.guardrailCases < summary.totalCases && (
            <Typography variant="bodySmall" color="text.secondary" data-testid="run-trajectory-guardrail-partial">
              {t('features.agentEvaluation.trajectory.guardrailPartial', 'Guardrail decisions were read for {{measured}} of {{total}} case(s).', {
                measured: summary.guardrailCases,
                total: summary.totalCases,
              })}
            </Typography>
          )}
          {summary.cases.map((entry) => (
            <CaseRow key={entry.caseId} entry={entry} />
          ))}
        </>
      )}
    </Box>
  );
}
