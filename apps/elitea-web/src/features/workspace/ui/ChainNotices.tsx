/**
 * Where a turn stopped, under its transcript: the 512 tool-call stop with its Continue, the goal check of the turn
 * (ADR-0045 decision 11) and the end of an unattended chain (ADR-0045). A turn that the host continued by itself (`turn_continued`) offers no Continue. Also the marker
 * that stands in place of the person's prompt above a turn that the host started by itself.
 */
import Alert, { type AlertColor } from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Divider from '@mui/material/Divider';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { GoalCheckNotice } from '@/entities/run-brief';
import type { ChainEndedPayload } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { TurnView } from '../model/turnReducer';

type OnContinue = (() => void) | undefined;

function ContinueButton({ onContinue }: { onContinue: OnContinue }): React.JSX.Element | null {
  if (onContinue === undefined) return null;
  return (
    <Button color="inherit" size="small" onClick={onContinue} data-testid="turn-continue">
      {t('workspace.toolCallLimit.continue', 'Continue')}
    </Button>
  );
}

/** The turn stopped at the 512 tool calls one turn can save: "Continue" goes on in a new turn. */
function ToolCallLimit({ onContinue }: { onContinue: OnContinue }): React.JSX.Element {
  return (
    <Alert severity="info" action={<ContinueButton onContinue={onContinue} />} data-testid="turn-tool-call-limit">
      {t('workspace.toolCallLimit.message', 'The agent made the most tool calls one turn can save. Continue goes on in a new turn.')}
    </Alert>
  );
}

const SEVERITY: Record<ChainEndedPayload['outcome'], AlertColor> = { completed: 'success', failed: 'error', waiting: 'info', limit_reached: 'warning', cancelled: 'info' };

function outcomeText(outcome: ChainEndedPayload['outcome']): string {
  switch (outcome) {
    case 'completed':
      return t('workspace.chain.completed', 'completed');
    case 'failed':
      return t('workspace.chain.failed', 'failed');
    case 'waiting':
      return t('workspace.chain.waiting', 'waiting for you. Click Continue to go on.');
    case 'limit_reached':
      return t('workspace.chain.limitReached', 'chain limit reached');
    case 'cancelled':
      return t('workspace.chain.cancelled', 'cancelled');
  }
}

function limitText(limit: ChainEndedPayload['limit']): string {
  switch (limit) {
    case 'continuations':
      return t('workspace.chain.limitContinuations', 'The run used all its automatic continuations.');
    case 'spend':
      return t('workspace.chain.limitSpend', 'The run reached its spend limit.');
    case 'deadline':
      return t('workspace.chain.limitDeadline', 'The run reached its deadline.');
    case undefined:
      return '';
  }
}

/** The chain's last turn: how it ended, which limit ended it, and Continue when the chain waits for the person. */
function ChainEnded({ ended, onContinue }: { ended: ChainEndedPayload; onContinue: OnContinue }): React.JSX.Element {
  const detail = [limitText(ended.limit), ended.message ?? ''].filter((part) => part !== '').join(' ');
  return (
    <Alert severity={SEVERITY[ended.outcome]} action={ended.outcome === 'waiting' ? <ContinueButton onContinue={onContinue} /> : undefined} data-testid="chain-ended">
      {t('workspace.chain.ended', 'Unattended run ended: {{outcome}}', { outcome: outcomeText(ended.outcome) })}
      {detail !== '' && ` ${detail}`}
    </Alert>
  );
}

/** The stop notices of a turn. Continue shows once: on the tool-call stop, else on a chain that waits. A goal check comes before the chain's end. */
export function StopNotices({ view, onContinue }: { view: TurnView; onContinue: OnContinue }): React.JSX.Element {
  const toolCallStop = view.note === 'tool_call_limit_reached' && view.continuedTo === undefined;
  return (
    <>
      {toolCallStop && <ToolCallLimit onContinue={onContinue} />}
      <GoalCheckNotice check={view.goalCheck} />
      {view.chainEnded !== undefined && <ChainEnded ended={view.chainEnded} onContinue={toolCallStop ? undefined : onContinue} />}
    </>
  );
}

/** In place of the person's prompt: the host started this turn by itself, the n-th time in its chain (`resumed`: after an app restart). */
export function ContinuedMarker({ index, resumed = false }: { index: number; resumed?: boolean }): React.JSX.Element {
  return (
    <Divider data-testid="turn-continued-marker" textAlign="center">
      <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {resumed
          ? t('workspace.chain.resumed', 'Resumed after restart ({{n}})', { n: String(index) })
          : t('workspace.chain.continued', 'Continued automatically ({{n}})', { n: String(index) })}
      </Typography>
    </Divider>
  );
}
