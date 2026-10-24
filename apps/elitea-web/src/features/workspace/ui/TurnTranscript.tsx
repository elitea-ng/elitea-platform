/**
 * The live transcript of one turn: streamed text (Markdown, as in chat), each step's reasoning as a
 * folded "Thinking" block (`ThinkingBlock`), tool rows (local or remote, with a collapsible result),
 * each `spawn_agent` call's sub-agents under its row (`SubAgentLanes`), a background process's output under its
 * `run_command` row (`ToolProcessOutput`), the status line and the cancel button.
 */
import { useState, type MouseEvent } from 'react';

import CheckIcon from '@mui/icons-material/Check';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CloseIcon from '@mui/icons-material/Close';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import Chip from '@mui/material/Chip';
import CircularProgress from '@mui/material/CircularProgress';
import Collapse from '@mui/material/Collapse';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { Markdown } from '@/shared/ui/Markdown';

import { lanesOf } from '../model/subAgentEvents';
import { isTerminalPhase, type TranscriptItem, type TurnView } from '../model/turnReducer';
import { SubAgentLanes } from './SubAgentLanes';
import { ThinkingBlock } from './ThinkingBlock';
import { ToolProcessOutput } from './ToolProcessOutput';

type ToolItem = Extract<TranscriptItem, { type: 'tool' }>;

function phaseLabel(phase: NonNullable<TurnView['phase']>): string {
  switch (phase) {
    case 'resolving':
      return t('workspace.phase.resolving', 'Preparing');
    case 'starting':
      return t('workspace.phase.starting', 'Starting');
    case 'running':
      return t('workspace.phase.running', 'Working');
    case 'committing':
      return t('workspace.phase.committing', 'Saving to the conversation');
    case 'done':
      return t('workspace.phase.done', 'Done');
    case 'cancelled':
      return t('workspace.phase.cancelled', 'Cancelled');
    case 'error':
      return t('workspace.phase.error', 'Failed');
  }
}

function ToolStatus({ result }: { result: ToolItem['result'] }): React.JSX.Element {
  if (result === undefined) {
    return (
      <Box component="output" sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
        <CircularProgress size="0.75rem" />
        <Typography variant="bodySmall" component="span">
          {t('workspace.tool.running', 'Running')}
        </Typography>
      </Box>
    );
  }
  return (
    <Box
      component="span"
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: 0.5,
        color: result.ok ? theme.vars.palette.success.main : theme.vars.palette.error.main,
        '& svg': { width: '0.875rem', height: '0.875rem' },
      })}
    >
      {result.ok ? <CheckIcon /> : <CloseIcon />}
      <Typography variant="bodySmall" component="span">
        {result.ok ? t('workspace.tool.succeeded', 'Succeeded') : t('workspace.tool.failed', 'Failed')}
      </Typography>
    </Box>
  );
}

/** One tool call as a single line ("▸ tool  args  ✓"); the result unfolds under it. */
function ToolRow({ item }: { item: ToolItem }): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const { result } = item;
  const resultId = `tool-result-${item.callId}`;
  const line = (
    <>
      <Box aria-hidden sx={{ display: 'flex', '& svg': { width: '1rem', height: '1rem' } }}>
        {result === undefined ? <ChevronRightIcon sx={{ opacity: 0.4 }} /> : open ? <ExpandMoreIcon /> : <ChevronRightIcon />}
      </Box>
      <Typography variant="labelSmall" component="span" sx={(theme: Theme) => ({ fontFamily: theme.typography.fontFamilyMono, flexShrink: 0 })}>
        {item.tool}
      </Typography>
      <Typography
        variant="bodySmall"
        component="span"
        sx={(theme: Theme) => ({
          color: theme.vars.palette.text.secondary,
          flex: 1,
          minWidth: 0,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
          textAlign: 'left',
        })}
      >
        {item.argsSummary}
      </Typography>
      <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, flexShrink: 0 })}>
        {item.remote ? t('workspace.tool.remote', 'Remote') : t('workspace.tool.local', 'Local')}
      </Typography>
      <ToolStatus result={result} />
    </>
  );
  const lineSx = (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: 1,
    width: '100%',
    paddingX: 0.5,
    paddingY: 0.25,
    borderRadius: theme.vars.shape.radiusSm,
  });
  return (
    <Box data-testid="tool-row">
      {result === undefined ? (
        <Box sx={lineSx}>{line}</Box>
      ) : (
        <ButtonBase
          aria-expanded={open}
          aria-controls={resultId}
          onClick={() => setOpen((value) => !value)}
          sx={(theme: Theme) => ({ ...lineSx(theme), '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover } })}
        >
          {line}
        </ButtonBase>
      )}
      {result !== undefined && (
        <Collapse in={open} unmountOnExit>
          <Typography
            id={resultId}
            variant="bodySmall"
            component="pre"
            sx={(theme: Theme) => ({
              whiteSpace: 'pre-wrap',
              margin: 0,
              marginLeft: 3,
              marginTop: 0.5,
              padding: 1,
              fontFamily: theme.typography.fontFamilyMono,
              borderLeft: `2px solid ${theme.vars.palette.divider}`,
              color: theme.vars.palette.text.secondary,
            })}
          >
            {result.summary}
            {result.truncated && ` ${t('workspace.tool.truncated', '(output truncated)')}`}
          </Typography>
        </Collapse>
      )}
      {item.process !== undefined && <ToolProcessOutput process={item.process} />}
    </Box>
  );
}

export interface TurnTranscriptProps {
  view: TurnView;
  busy: boolean;
  onCancel: () => void;
  /** The turn's commit failed after `done`: send it again (absent: the notice offers no Retry). */
  onRetryCommit?: (executionId: string) => Promise<void>;
  /** Right-click on the agent's text (the desktop's native context menu: Copy). */
  onTextContextMenu?: (text: string, event: MouseEvent) => void;
  /** The turn failed in a way another try may mend: "Retry" sends the message again (`/retry`); absent: not offered. */
  onRetryTurn?: () => void;
}

/**
 * The failures another try may mend: the host gave up on its own model retries (a cut or stalled stream, the gateway
 * unreachable, slow or rate limited). A refusal (budget, sign-in, a model the project may not use) is not offered one.
 */
const RETRYABLE = new Set(['model_gateway.incomplete_stream', 'model_gateway.unavailable', 'model_gateway.upstream_timeout', 'model_gateway.rate_limited', 'model_gateway.conflict']);

/** The turn's error, with "Retry" when another try may mend it. */
function TurnError({ error, onRetry }: { error: NonNullable<TurnView['error']>; onRetry?: (() => void) | undefined }): React.JSX.Element {
  const action =
    onRetry === undefined || !RETRYABLE.has(error.code) ? undefined : (
      <Button color="inherit" size="small" onClick={onRetry} data-testid="turn-error-retry">
        {t('workspace.turnError.retry', 'Retry')}
      </Button>
    );
  return (
    <Alert severity="error" action={action}>
      {error.message}
    </Alert>
  );
}

/** The answer is on screen, but not in the conversation: the host's reason, and "Retry". */
function CommitFailed({ failed, onRetry }: { failed: NonNullable<TurnView['commitFailed']>; onRetry?: (executionId: string) => Promise<void> }): React.JSX.Element {
  const [retrying, setRetrying] = useState(false);
  const action =
    onRetry === undefined ? undefined : (
      <Button
        color="inherit"
        size="small"
        disabled={retrying}
        onClick={() => {
          setRetrying(true);
          void onRetry(failed.executionId).finally(() => setRetrying(false));
        }}
      >
        {t('workspace.commitFailed.retry', 'Retry')}
      </Button>
    );
  return (
    <Alert severity="warning" data-testid="commit-failed" action={action}>
      {failed.message}
    </Alert>
  );
}

/** A failed model call is made again (`model_retry`): until the model streams again or the turn ends. */
function RetryNotice({ view }: { view: TurnView }): React.JSX.Element | null {
  if (view.retrying === undefined || view.error !== undefined) return null;
  const { attempt, max_attempts: max } = view.retrying;
  return (
    <Alert severity="warning" data-testid="turn-retrying">
      {t('workspace.modelRetrying', 'The model call failed; retrying ({{attempt}} of {{max}}).', { attempt: String(attempt), max: String(max) })}
    </Alert>
  );
}

export function TurnTranscript({ view, busy, onCancel, onRetryCommit, onTextContextMenu, onRetryTurn }: TurnTranscriptProps): React.JSX.Element {
  const showStatus = view.phase !== null;
  return (
    <Box data-testid="turn-transcript" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      {view.items.map((item) =>
        item.type === 'text' ? (
          // The chat's own renderer; raw HTML from the model is dropped, not rendered.
          <Box key={item.key} onContextMenu={onTextContextMenu === undefined ? undefined : (event: MouseEvent) => onTextContextMenu(item.text, event)}>
            <Markdown renderHtml={false} data-testid="turn-text">
              {item.text}
            </Markdown>
          </Box>
        ) : item.type === 'thinking' ? (
          <ThinkingBlock key={item.key} item={item} />
        ) : (
          <Box key={item.key}>
            <ToolRow item={item} />
            <SubAgentLanes lanes={lanesOf(view.subAgents, item.callId)} ToolRow={ToolRow} />
          </Box>
        ),
      )}
      {view.error !== undefined && <TurnError error={view.error} onRetry={onRetryTurn} />}
      <RetryNotice view={view} />
      {view.commitFailed !== undefined && <CommitFailed key={view.commitFailed.seq} failed={view.commitFailed} {...(onRetryCommit === undefined ? {} : { onRetry: onRetryCommit })} />}
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}>
        {view.projectInstructions !== undefined && view.projectInstructions.length > 0 && (
          <Tooltip title={view.projectInstructions.join(', ')}>
            <Chip
              size="small"
              variant="outlined"
              data-testid="agents-md-applied"
              label={t('workspace.agentsMdApplied', 'AGENTS.md applied')}
              aria-description={view.projectInstructions.join(', ')}
            />
          </Tooltip>
        )}
        {showStatus && view.phase !== null && (
          <Typography variant="bodySmall" component="output" data-testid="turn-status">
            {phaseLabel(view.phase)}
            {view.message !== undefined && ` — ${view.message}`}
          </Typography>
        )}
        {busy && !isTerminalPhase(view.phase) && (
          <Button size="small" color="error" variant="outlined" onClick={onCancel}>
            {t('workspace.cancel', 'Cancel')}
          </Button>
        )}
      </Box>
    </Box>
  );
}
