/**
 * The goal check of a run (ADR-0045 decision 11), under the checked answer:
 *
 *   Done · 3 of 3 checks · model-reviewed   (the chain ends completed)
 *   Not done yet: {reason}                  (a continuation follows)
 *   Blocked on: {reason}                    (the chain ends failed)
 *   Not checked                             (the check failed; the chain ends completed)
 *
 * One quiet line, not an alert: the chain's own end notice follows it. The
 * reason is a model's words and shows as plain text.
 */
import AutorenewIcon from '@mui/icons-material/Autorenew';
import BlockOutlinedIcon from '@mui/icons-material/BlockOutlined';
import RemoveDoneIcon from '@mui/icons-material/RemoveDone';
import TaskAltIcon from '@mui/icons-material/TaskAlt';
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { GoalCheckOutcome } from '@/shared/desktop/unattendedIpc';
import { t } from '@/shared/i18n';

import type { GoalCheckView } from '../model/runBrief';

/** The notice's text for `check`. */
export function goalCheckText(check: Pick<GoalCheckView, 'outcome' | 'met' | 'total' | 'reason'>): string {
  switch (check.outcome) {
    case 'met':
      return t('unattended.check.met', 'Done · {{met}} of {{total}} checks · model-reviewed', { met: String(check.met), total: String(check.total) });
    case 'not_met':
      return check.reason === undefined ? t('unattended.check.notMetNoReason', 'Not done yet') : t('unattended.check.notMet', 'Not done yet: {{reason}}', { reason: check.reason });
    case 'blocked':
      return check.reason === undefined ? t('unattended.check.blockedNoReason', 'Blocked') : t('unattended.check.blocked', 'Blocked on: {{reason}}', { reason: check.reason });
    case 'not_checked':
      return t('unattended.check.notChecked', 'Not checked');
  }
}

const ICONS: Record<GoalCheckOutcome, typeof TaskAltIcon> = {
  met: TaskAltIcon,
  not_met: AutorenewIcon,
  blocked: BlockOutlinedIcon,
  not_checked: RemoveDoneIcon,
};

const toneOf = (theme: Theme, outcome: GoalCheckOutcome): string => {
  switch (outcome) {
    case 'met':
      return theme.vars.palette.success.main;
    case 'blocked':
      return theme.vars.palette.error.main;
    case 'not_met':
    case 'not_checked':
      return theme.vars.palette.text.metrics;
  }
};

export function GoalCheckNotice({ check }: { check: GoalCheckView | undefined }): React.JSX.Element | null {
  if (check === undefined) return null;
  const Icon = ICONS[check.outcome];
  return (
    <Box data-testid="goal-check-notice" data-outcome={check.outcome} sx={{ display: 'flex', alignItems: 'flex-start', gap: 0.75 }}>
      <Icon aria-hidden sx={(theme: Theme) => ({ color: toneOf(theme, check.outcome), width: '1rem', height: '1rem', flexShrink: 0, marginTop: 0.25 })} />
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: check.outcome === 'blocked' ? theme.vars.palette.error.main : theme.vars.palette.text.secondary, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' })}>
        {goalCheckText(check)}
      </Typography>
    </Box>
  );
}
