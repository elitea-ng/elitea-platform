/**
 * The live transcript of one turn: streamed text (Markdown, as in chat), each step's reasoning as a
 * folded "Thinking" block (`ThinkingBlock`), tool rows (local or remote, with a collapsible result),
 * each `spawn_agent` call's sub-agents under its row (`SubAgentLanes`), a background process's output under its
 * `run_command` row (`ToolProcessOutput`), a message sent while the turn ran (`SteerMessage`), where the turn stopped
 * (`StopNotices`: the tool-call stop, the goal check, the end of an unattended chain), the chain's run brief above its
 * first turn (`RunBriefCard`) and the status line, quiet
 * text with a small spinner while the turn runs. Stop is the composer's (its round button, or Esc Esc).
 */
import { useState, type ComponentType, type MouseEvent } from 'react';

import CheckIcon from '@mui/icons-material/Check';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CloseIcon from '@mui/icons-material/Close';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import CircularProgress from '@mui/material/CircularProgress';
import Collapse from '@mui/material/Collapse';
import Link from '@mui/material/Link';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { RunBriefCard } from '@/entities/run-brief';
import { t } from '@/shared/i18n';
import { classifyRuntimeFailure, delegationRefusalExplainsFailure, refusalCategoryTitle, refusalFallbackMessage } from '@/shared/lib/runtimeFailure';
import { Markdown } from '@/shared/ui/Markdown';
import { conversationBubbleSx } from '@/shared/ui/conversation-column';

import { lanesOf } from '../model/subAgentEvents';
import { isTerminalPhase, type TranscriptItem, type TurnView } from '../model/turnReducer';
import { AnsweringAgent, ContinuedMarker, StopNotices } from './ChainNotices';
import { SteerMessage } from './SteerMessage';
import { SubAgentLanes, type SubAgentAvatar } from './SubAgentLanes';
import { DelegationNotices } from '@/shared/ui/delegation-notices';
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
      {result?.thumbnail !== undefined && (
        <Box
          component="img"
          src={result.thumbnail}
          alt={t('workspace.tool.screenshot', 'Screenshot the agent took')}
          data-testid="tool-row-thumbnail"
          sx={(theme: Theme) => ({
            display: 'block',
            marginLeft: 3,
            marginTop: 0.5,
            maxWidth: 160,
            maxHeight: 160,
            borderRadius: theme.vars.shape.radiusSm,
            border: `1px solid ${theme.vars.palette.divider}`,
          })}
        />
      )}
      {item.link !== undefined && (
        // `target="_blank"`: the desktop hands it to the system browser (`installExternalLinks`), never a webview.
        <Link href={item.link.href} target="_blank" rel="noopener noreferrer" variant="bodySmall" data-testid="tool-row-link" sx={{ display: 'block', marginLeft: 3, fontWeight: 600 }}>
          {item.link.label}
        </Link>
      )}
      {item.process !== undefined && <ToolProcessOutput process={item.process} />}
    </Box>
  );
}

export interface TurnTranscriptProps {
  view: TurnView;
  /** The turn runs here: the status line shows a spinner until the turn ends. */
  busy: boolean;
  /** The turn's commit failed after `done`: send it again (absent: the notice offers no Retry). */
  onRetryCommit?: (executionId: string) => Promise<void>;
  /** Right-click on the agent's text (the desktop's native context menu: Copy). */
  onTextContextMenu?: (text: string, event: MouseEvent) => void;
  /** The turn failed in a way another try may mend: "Retry" sends the message again (`/retry`); absent: not offered. */
  onRetryTurn?: () => void;
  /** The turn stopped at the tool calls one turn can save, or its unattended chain waits: "Continue" starts a new turn; absent: not offered. */
  onContinue?: () => void;
  /** The host started this turn by itself (an unattended chain), the n-th time: its marker shows first, in place of the person's prompt. */
  continuation?: number | undefined;
  /** Each sub-agent lane's avatar (the cloud transcript's temporary participants); absent: none. */
  subAgentAvatar?: SubAgentAvatar | undefined;
  /** What a failed tool call offers under its row, by error code (a trusted-folder offer on `sandbox_unavailable`); `null`: nothing. */
  toolOffer?: ((code: string) => React.ReactNode) | undefined;
  /** The turn's side talk (a hand-off, consults) and a loop guard's stop, drawn as the chat draws them; absent: not shown. */
  sideTalk?: ComponentType<{ view: TurnView }> | undefined;
}

function SideTalkSlot({ view, Slot }: { view: TurnView; Slot: TurnTranscriptProps['sideTalk'] }): React.ReactNode {
  return Slot === undefined ? null : <Slot view={view} />;
}

/** A failed call's offer, when the row has an error code and the page offers something for it. */
function ToolOffer({ item, offer }: { item: ToolItem; offer: TurnTranscriptProps['toolOffer'] }): React.ReactNode {
  const code = item.result?.code;
  return code === undefined || offer === undefined ? null : offer(code);
}

/**
 * The failures another try may mend: the host gave up on its own model retries (a cut or stalled stream, the gateway
 * unreachable, slow or rate limited). A refusal (budget, sign-in, a model the project may not use) is not offered one.
 */
const RETRYABLE = new Set(['model_gateway.incomplete_stream', 'model_gateway.unavailable', 'model_gateway.upstream_timeout', 'model_gateway.rate_limited']);

/** The turn's error, with "Retry" when another try may mend it. Typed refusals show a refusal status instead of a generic error. */
function TurnError({ error, onRetry }: { error: NonNullable<TurnView['error']>; onRetry?: (() => void) | undefined }): React.JSX.Element {
  const classification = classifyRuntimeFailure(error.code);
  if (classification.kind === 'refusal') {
    const reason = error.message === error.code ? refusalFallbackMessage(classification.category) : error.message;
    return (
      <Alert severity="warning" data-testid="turn-refusal" data-refusal-category={classification.category}>
        <AlertTitle>{refusalCategoryTitle(classification.category)}</AlertTitle>
        {reason}
      </Alert>
    );
  }
  if (classification.kind === 'cancelled') {
    return (
      <Alert severity="info" data-testid="turn-cancelled">
        <AlertTitle>{t('runtimeFailure.cancelledTitle', 'Run stopped')}</AlertTitle>
        {error.message}
      </Alert>
    );
  }
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

export function TurnTranscript({ view, busy, onRetryCommit, onTextContextMenu, onRetryTurn, onContinue, continuation, subAgentAvatar, toolOffer, sideTalk }: TurnTranscriptProps): React.JSX.Element {
  const showStatus = view.phase !== null;
  return (
    <Box data-testid="turn-transcript" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <ContinuedMarker index={continuation} resumed={view.resumed === true} />
      <AnsweringAgent agent={view.agent} />
      <RunBriefCard value={view.runBrief} />
      {view.items.map((item) =>
        item.type === 'text' ? (
          // The chat's own renderer; raw HTML from the model is dropped, not rendered.
          <Box key={item.key} sx={conversationBubbleSx(false)} onContextMenu={onTextContextMenu === undefined ? undefined : (event: MouseEvent) => onTextContextMenu(item.text, event)}>
            <Markdown renderHtml={false} data-testid="turn-text">
              {item.text}
            </Markdown>
          </Box>
        ) : item.type === 'thinking' ? (
          <ThinkingBlock key={item.key} item={item} />
        ) : item.type === 'steer' ? (
          <SteerMessage key={item.key} text={item.text} />
        ) : (
          <Box key={item.key}>
            <ToolRow item={item} />
            <ToolOffer item={item} offer={toolOffer} />
            <SubAgentLanes lanes={lanesOf(view.subAgents, item.callId)} ToolRow={ToolRow} Avatar={subAgentAvatar} />
          </Box>
        ),
      )}
      <DelegationNotices notices={view.delegationNotices} />
      <SideTalkSlot view={view} Slot={sideTalk} />
      {view.error !== undefined && !delegationRefusalExplainsFailure(view.error.code, view.delegationNotices, view.error.message) && <TurnError error={view.error} onRetry={onRetryTurn} />}
      <RetryNotice view={view} />
      <StopNotices view={view} onContinue={onContinue} />
      {view.commitFailed !== undefined && <CommitFailed key={view.commitFailed.seq} failed={view.commitFailed} {...(onRetryCommit === undefined ? {} : { onRetry: onRetryCommit })} />}
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}>
        {showStatus && view.phase !== null && (
          <Typography
            variant="bodySmall"
            component="output"
            data-testid="turn-status"
            sx={(theme: Theme) => ({ display: 'inline-flex', alignItems: 'center', gap: 0.75, color: theme.vars.palette.text.metrics })}
          >
            {busy && !isTerminalPhase(view.phase) && <CircularProgress size="0.75rem" color="inherit" aria-hidden />}
            {phaseLabel(view.phase)}
            {view.message !== undefined && ` — ${view.message}`}
          </Typography>
        )}
      </Box>
    </Box>
  );
}
