/**
 * The pieces of a thread screen (`WorkspaceSessionPage`): the folder title,
 * the panel toggle, your prompt, the empty thread's intro, the exchanges,
 * and the notices above the composer.
 */
import { useMemo } from 'react';

import { Link } from '@tanstack/react-router';

import VerticalSplitOutlinedIcon from '@mui/icons-material/VerticalSplitOutlined';
import Alert from '@mui/material/Alert';
import Badge from '@mui/material/Badge';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { composer, describeWorkspaceError, replayView, TurnTranscript, type WorkspaceTurn } from '@/features/workspace';
import type { CustomCommand, StoredTurn, Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { Markdown } from '@/shared/ui/Markdown';
import { copyText, modKey, selectInspectorShown, useContextMenu, useDesktopLayout } from '@/widgets/desktop-shell';

import type { ForkPoint, SessionFork } from './useForkThread';
import type { ComposerSkill } from './useAgentSkills';
import type { SessionCommands } from './useSessionCommands';
import type { ServerMessage, ThreadHistory } from './useThreadHistory';

/** Links in the thread read as quiet app links, not underlined web ones. */
const linkSx = (theme: Theme) => ({
  '& a': { ...theme.typography.labelSmall, color: theme.vars.palette.primary.main, textDecoration: 'none' },
  '& a:hover': { textDecoration: 'underline' },
});

export const COLUMN = { width: '100%', maxWidth: '46rem', marginX: 'auto', paddingX: 3, boxSizing: 'border-box' } as const;

interface HelpNoticeProps {
  onClose: () => void;
  customCommands: readonly CustomCommand[];
  skills: readonly ComposerSkill[];
}

function HelpRow({ name, text }: { name: string; text: string }): React.JSX.Element {
  return (
    <li>
      <Typography component="span" variant="labelMedium">
        {name}
      </Typography>
      {` — ${text}`}
    </li>
  );
}

/** `/help`: the host commands, the custom commands (with their source), the agent's skills, the project's, then "@". */
function HelpNotice({ onClose, customCommands, skills }: HelpNoticeProps): React.JSX.Element {
  const entries = composer.slashMenu('', customCommands, skills);
  return (
    <Alert severity="info" onClose={onClose} data-testid="workspace-help">
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2 }}>
        {entries.map((entry) => {
          switch (entry.kind) {
            case 'host':
              return <HelpRow key={entry.command.id} name={entry.command.name} text={entry.command.description} />;
            case 'custom':
              return (
                <HelpRow
                  key={`command:${entry.command.name}`}
                  name={composer.customCommandToken(entry.command)}
                  text={`${composer.customCommandDetail(entry.command)} (${composer.customCommandSource(entry.command)})`}
                />
              );
            case 'skill':
              return (
                <HelpRow
                  key={`skill:${entry.skill.project === true ? 'project' : 'agent'}:${entry.skill.id}`}
                  name={composer.skillToken(entry.skill)}
                  text={entry.skill.description ?? (entry.skill.project === true ? t('workspace.command.projectSkill', 'A skill of this project') : t('workspace.command.skill', 'A skill of this agent'))}
                />
              );
          }
        })}
        <li>
          <Typography component="span" variant="labelMedium">
            @
          </Typography>
          {` — ${t('workspace.command.mention', 'Reference a file or folder of this workspace')}`}
        </li>
      </Box>
    </Alert>
  );
}

/** The message to show: the turn's refusal, else the send's failure, else a failed re-bind. */
export function firstError(startError: string | null, sendError: string | null, rebindError: unknown): string | null {
  if (startError !== null) return startError;
  if (sendError !== null) return sendError;
  return rebindError === null || rebindError === undefined ? null : describeWorkspaceError(rebindError);
}

/**
 * The native context menu of a message: Copy, and "Fork from here" when the
 * message can be a fork point (`fork`; `undefined` = not offered, `null` = offered, disabled).
 */
export function useCopyMenu(): (text: string, event: React.MouseEvent, fork?: (() => void) | null) => void {
  const showMenu = useContextMenu();
  return (text, event, fork) =>
    showMenu(event, [
      { id: 'copy', label: t('workspace.copy', 'Copy'), run: () => copyText(text) },
      ...(fork === undefined
        ? []
        : [{ separator: true } as const, { id: 'fork', label: t('workspace.fork.fromHere', 'Fork from Here'), enabled: fork !== null, run: () => fork?.() }]),
    ]);
}

/** A turn's fork point: its answer, the last message its `done` event names. */
function turnForkPoint(turnId: string, done: { messageIds: readonly string[] } | undefined): ForkPoint | null {
  const answer = done?.messageIds.at(-1);
  return answer === undefined || answer === '' ? null : { messageId: answer, turnId };
}

/** What a turn's context menu forks: `undefined` when the thread offers no fork, `null` when this turn cannot be one. */
const forkAction = (onFork: ((point: ForkPoint) => void) | undefined, point: ForkPoint | null): (() => void) | null | undefined =>
  onFork === undefined ? undefined : point === null ? null : () => onFork(point);

export function YourPrompt({ text, fork }: { text: string; fork?: (() => void) | null | undefined }): React.JSX.Element {
  const copyMenu = useCopyMenu();
  return (
    <Box
      data-testid="your-prompt"
      onContextMenu={(event) => copyMenu(text, event, fork)}
      sx={(theme: Theme) => ({
        alignSelf: 'flex-end',
        maxWidth: '85%',
        paddingX: 1.5,
        paddingY: 1,
        borderRadius: theme.vars.shape.radiusMd,
        background: theme.vars.palette.background.userInputBackground,
      })}
    >
      <Typography variant="bodyMedium" sx={{ whiteSpace: 'pre-wrap' }}>
        {text}
      </Typography>
    </Box>
  );
}

export function FolderTitle({ workspace }: { workspace: Workspace }): React.JSX.Element {
  return (
    <Box sx={{ minWidth: 0, flexShrink: 1, display: 'flex', alignItems: 'baseline', gap: 1 }}>
      <Typography component="h1" variant="labelMedium" sx={{ margin: 0, whiteSpace: 'nowrap', flexShrink: 0 }}>
        {workspace.name}
      </Typography>
      <Typography
        variant="bodySmall"
        title={workspace.path}
        sx={(theme: Theme) => ({ minWidth: '3rem', color: theme.vars.palette.text.metrics, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' })}
      >
        {workspace.path}
      </Typography>
    </Box>
  );
}

/** Opens the inspector when it is hidden; the dot says the thread's tabs have something to show. */
export function PanelToggle({ count }: { count: number }): React.JSX.Element {
  const open = useDesktopLayout(selectInspectorShown);
  const label = open ? t('desktop.inspector.hide', 'Hide inspector') : t('desktop.inspector.show', 'Show inspector');
  return (
    <Tooltip title={`${label} (${modKey()}⌥\\)`}>
      <IconButton size="small" aria-label={label} aria-pressed={open} onClick={() => useDesktopLayout.getState().toggleInspector()}>
        <Badge color="primary" variant="dot" invisible={count === 0}>
          <VerticalSplitOutlinedIcon fontSize="inherit" />
        </Badge>
      </IconButton>
    </Tooltip>
  );
}

/**
 * A thread with nothing to show above the composer: a new one, or one whose
 * earlier messages could not be read (`unavailable`: they are in the chat).
 */
export function ThreadIntro({
  workspace,
  conversationId,
  title,
  unavailable = false,
}: {
  workspace: Workspace;
  conversationId: string;
  title: string | undefined;
  unavailable?: boolean;
}): React.JSX.Element {
  const isNew = conversationId === '';
  return (
    <Box sx={{ paddingY: 6, textAlign: 'center', display: 'flex', flexDirection: 'column', gap: 1, alignItems: 'center' }}>
      <Typography variant="headingMedium" component="p">
        {isNew ? t('workspace.thread.newTitle', 'What should we do in {{name}}?', { name: workspace.name }) : (title ?? t('workspace.thread.continue', 'Continue this thread'))}
      </Typography>
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {unavailable
          ? t('workspace.thread.earlierHint', 'Earlier messages of this thread are in the conversation.')
          : t('workspace.thread.newHint', 'Type @ to add a file, / for commands and skills. The agent works in this folder, on this computer.')}
      </Typography>
      {unavailable && !isNew && (
        <Box sx={linkSx}>
          <Link to="/chat/$conversationId" params={{ conversationId }}>
            {t('workspace.openInChat', 'Open in chat')}
          </Link>
        </Box>
      )}
    </Box>
  );
}

const quietSx = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

/** One recorded turn: its prompt, then its events replayed as the live transcript showed them. */
function RecordedTurn({ stored, onFork }: { stored: StoredTurn; onFork: ((point: ForkPoint) => void) | undefined }): React.JSX.Element {
  const copyMenu = useCopyMenu();
  const view = useMemo(() => replayView(stored.events), [stored.events]);
  const fork = forkAction(onFork, stored.state === 'running' ? null : turnForkPoint(stored.turn_id, view.done));
  return (
    <Box data-testid="history-turn" sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <YourPrompt text={stored.prompt} fork={fork} />
      <TurnTranscript view={view} busy={false} onCancel={() => undefined} onTextContextMenu={(text, event) => copyMenu(text, event, fork)} />
      {stored.state === 'interrupted' && view.done === undefined && (
        <Typography variant="bodySmall" sx={quietSx}>
          {t('workspace.history.interrupted', 'This turn did not finish: the app closed while it ran.')}
        </Typography>
      )}
      {stored.events_truncated && (
        <Typography variant="bodySmall" sx={quietSx}>
          {t('workspace.history.truncated', 'Part of this turn was too long to keep on this computer.')}
        </Typography>
      )}
    </Box>
  );
}

function ServerTurn({ message, onFork }: { message: ServerMessage; onFork: ((point: ForkPoint) => void) | undefined }): React.JSX.Element {
  const copyMenu = useCopyMenu();
  // A thread this computer has no record of forks on the server only (no turn to copy).
  const fork = forkAction(onFork, { messageId: message.id });
  if (message.role === 'user') return <YourPrompt text={message.content} fork={fork} />;
  return (
    // The chat's own renderer; raw HTML from the model is dropped, not rendered.
    <Box onContextMenu={(event) => copyMenu(message.content, event, fork)}>
      <Markdown renderHtml={false} data-testid="history-answer">
        {message.content}
      </Markdown>
    </Box>
  );
}

/** The thread's earlier turns, above this session's: recorded on this computer, else the conversation's messages. */
export function EarlierTurns({
  history,
  shown,
  onFork,
}: {
  history: ThreadHistory;
  shown: ReadonlySet<string>;
  /** "Fork from here" on each message; absent = not offered. */
  onFork?: ((point: ForkPoint) => void) | undefined;
}): React.JSX.Element | null {
  switch (history.kind) {
    case 'loading':
      return (
        <Typography variant="bodySmall" component="output" sx={{ alignSelf: 'center' }}>
          {t('workspace.history.loading', 'Loading earlier messages')}
        </Typography>
      );
    case 'local':
      return (
        <>
          {history.turns
            .filter((stored) => !shown.has(stored.turn_id))
            .map((stored) => (
              <RecordedTurn key={stored.turn_id} stored={stored} onFork={onFork} />
            ))}
        </>
      );
    case 'server':
      return (
        <>
          {history.messages.map((message) => (
            <ServerTurn key={message.id} message={message} onFork={onFork} />
          ))}
        </>
      );
    case 'none':
    case 'unavailable':
      return null;
  }
}

/** Each turn started here, under the prompt that started it; the last one is live. ("Open in chat" is in the header.) */
export function Exchanges({
  turn,
  prompts,
  onFork,
}: {
  turn: WorkspaceTurn;
  prompts: readonly string[];
  /** "Fork from here" on each ended turn; absent = not offered. */
  onFork?: ((point: ForkPoint) => void) | undefined;
}): React.JSX.Element {
  const copyMenu = useCopyMenu();
  const exchanges = [
    ...turn.earlier.map((entry, index) => ({ key: entry.turnId, prompt: prompts[index], view: entry.view, live: false })),
    ...(turn.turnId === null ? [] : [{ key: turn.turnId, prompt: prompts[turn.earlier.length], view: turn.view, live: true }]),
  ];
  return (
    <>
      {exchanges.map((exchange) => {
        // A running turn is no fork point: the server would refuse it, and its record is not complete.
        const fork = forkAction(onFork, exchange.live && turn.busy ? null : turnForkPoint(exchange.key, exchange.view.done));
        return (
        <Box key={exchange.key} sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
          {exchange.prompt !== undefined && <YourPrompt text={exchange.prompt} fork={fork} />}
          <TurnTranscript
            view={exchange.view}
            busy={exchange.live && turn.busy}
            onCancel={() => void turn.cancel()}
            onRetryCommit={(executionId) => turn.retryCommit(exchange.key, executionId)}
            onTextContextMenu={(text, event) => copyMenu(text, event, fork)}
          />
        </Box>
        );
      })}
    </>
  );
}

/** What the fork that opened this thread copied, and why the last fork from here did not happen. */
export function ForkNotices({ fork }: { fork: SessionFork }): React.JSX.Element {
  return (
    <>
      {fork.notice !== null && (
        <Alert severity={fork.notice.severity} onClose={fork.dismissNotice} data-testid="fork-notice">
          {fork.notice.text}
        </Alert>
      )}
      {fork.error !== null && (
        <Alert severity="error" onClose={fork.clearError} data-testid="fork-error">
          {fork.error}
        </Alert>
      )}
    </>
  );
}

interface SessionNoticesProps {
  error: string | null;
  commands: SessionCommands;
  /** What `/help` lists after the host commands. */
  customCommands: readonly CustomCommand[];
  skills: readonly ComposerSkill[];
}

export function SessionNotices({ error, commands, customCommands, skills }: SessionNoticesProps): React.JSX.Element {
  return (
    <>
      {error !== null && <Alert severity="error">{error}</Alert>}
      {commands.notice === 'help' && <HelpNotice onClose={commands.closeNotice} customCommands={customCommands} skills={skills} />}
      {commands.notice === 'nothingToUndo' && (
        <Alert severity="info" onClose={commands.closeNotice}>
          {t('workspace.undoNothing', 'The last turn here changed no files.')}
        </Alert>
      )}
      {commands.notice === 'modelFixed' && (
        <Alert severity="info" onClose={commands.closeNotice} data-testid="workspace-model-fixed">
          {t('workspace.model.fixed', 'This agent runs on its own model here: it pins its model, or this folder’s settings keep it.')}
        </Alert>
      )}
      {commands.notice === 'compactNext' && (
        <Alert severity="info" onClose={commands.closeNotice} data-testid="workspace-compact-next">
          {t('workspace.compactNext', 'Your next message first summarizes this conversation, so the agent continues with more room.')}
        </Alert>
      )}
    </>
  );
}
