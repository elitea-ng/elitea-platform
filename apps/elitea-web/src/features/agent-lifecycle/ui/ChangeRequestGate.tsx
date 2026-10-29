import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentChangeRequest, ReleaseGate, ReleaseGateRegressedCase } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useReleaseGate } from '../api/governanceApi';
import { formatUser, formatWhen } from './governanceLabels';

/**
 * The release gate of a change request (ADR-0040 decision 4): its state, why
 * it failed, and the regression cases BY NAME. The change request carries the
 * gate's state and failure codes; the gate route carries the detail, so it is
 * read too once the request names a gate.
 */
export interface ChangeRequestGateProps {
  readonly projectId: string;
  readonly applicationId: number;
  readonly request: AgentChangeRequest;
}

export function ChangeRequestGate({ projectId, applicationId, request }: ChangeRequestGateProps): ReactNode {
  const gate = useReleaseGate(projectId, applicationId, request.gate_id);
  if (request.gate_id === null || request.gate === undefined) {
    return (
      <Typography
        variant="bodySmall"
        color="text.secondary"
        data-testid="change-request-gate"
      >
        {t(
          'features.agentLifecycle.gate.none',
          'No release gate: the agent has no gate suite, or the request is not submitted yet.',
        )}
      </Typography>
    );
  }
  const summary = request.gate;
  return (
    <Box
      sx={stackSx}
      data-testid="change-request-gate"
    >
      <Box sx={rowSx}>
        <Typography variant="labelSmall">{t('features.agentLifecycle.gate.title', 'Release gate')}</Typography>
        <Chip
          size="small"
          color={gateColor(summary.state, summary.overridden)}
          label={gateStateLabel(summary.state, summary.overridden)}
          data-testid="change-request-gate-state"
        />
      </Box>
      {gate.data === undefined ? (
        summary.failures.length > 0 && <Typography variant="bodySmall">{summary.failures.join(', ')}</Typography>
      ) : (
        <GateDetail gate={gate.data} />
      )}
      {request.overridden_at !== null && (
        <Typography
          variant="bodySmall"
          color="text.secondary"
        >
          {t('features.agentLifecycle.gate.overriddenBy', 'Overridden by {{who}} on {{when}}: {{reason}}', {
            who: formatUser(request.overridden_by),
            when: formatWhen(request.overridden_at),
            reason: request.override_reason,
          })}
        </Typography>
      )}
    </Box>
  );
}

function GateDetail({ gate }: { readonly gate: ReleaseGate }): ReactNode {
  const cases = regressedCases(gate);
  return (
    <>
      {gate.failures.length > 0 && (
        <Box
          component="ul"
          sx={listSx}
          data-testid="change-request-gate-failures"
        >
          {gate.failures.map((failure, index) => (
            <Typography
              key={`${failure.code}:${index}`}
              component="li"
              variant="bodySmall"
            >
              {failure.message === '' ? failure.code : failure.message}
            </Typography>
          ))}
        </Box>
      )}
      {cases.length > 0 && (
        <Box data-testid="change-request-gate-regressions">
          <Typography variant="labelSmall">
            {t('features.agentLifecycle.gate.regressedCases', 'Regressed cases')}
          </Typography>
          <Box
            component="ul"
            sx={listSx}
          >
            {cases.map((item) => (
              <Typography
                key={item.case_id}
                component="li"
                variant="bodySmall"
              >
                {item.name === '' ? t('features.agentLifecycle.gate.deletedCase', '(deleted case)') : item.name}
                {item.dimension_ids.length > 0 && ` · ${item.dimension_ids.join(', ')}`}
              </Typography>
            ))}
          </Box>
        </Box>
      )}
    </>
  );
}

/** The regressed cases of every suite, once each. */
function regressedCases(gate: ReleaseGate): ReleaseGateRegressedCase[] {
  const seen = new Map<string, ReleaseGateRegressedCase>();
  for (const suite of gate.outcome) {
    for (const item of suite.regressed_cases) seen.set(item.case_id, item);
  }
  for (const failure of gate.failures) {
    for (const item of failure.cases ?? []) seen.set(item.case_id, item);
  }
  return [...seen.values()];
}

function gateColor(state: string, overridden: boolean): 'default' | 'success' | 'error' | 'warning' {
  if (overridden) return 'warning';
  if (state === 'passed') return 'success';
  if (state === 'failed') return 'error';
  return 'default';
}

function gateStateLabel(state: string, overridden: boolean): string {
  if (overridden) return t('features.agentLifecycle.gate.overridden', 'Failed, overridden');
  switch (state) {
    case 'pending':
      return t('features.agentLifecycle.gate.pending', 'Evaluating');
    case 'passed':
      return t('features.agentLifecycle.gate.passed', 'Passed');
    case 'failed':
      return t('features.agentLifecycle.gate.failed', 'Failed');
    default:
      return state;
  }
}

const stackSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
const listSx: SxProps<Theme> = { margin: 0, paddingLeft: '1.25rem' };
