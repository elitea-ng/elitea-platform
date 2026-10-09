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
import type { StoredTurn, Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { Markdown } from '@/shared/ui/Markdown';
import { modKey, useDesktopLayout } from '@/widgets/desktop-shell';

import type { SessionCommands } from './useSessionCommands';
import type { ServerMessage, ThreadHistory } from './useThreadHistory';

/** Links in the thread read as quiet app links, not underlined web ones. */
const linkSx = (theme: Theme) => ({
  '& a': { ...theme.typography.labelSmall, color: theme.vars.palette.primary.main, textDecoration: 'none' },
  '& a:hover': { textDecoration: 'underline' },
});

export const COLUMN = { width: '100%', maxWidth: '46rem', marginX: 'auto', paddingX: 3, boxSizing: 'border-box' } as const;

function HelpNotice({ onClose }: { onClose: () => void }): React.JSX.Element {
  return (
    <Alert severity="info" onClose={onClose} data-testid="workspace-help">
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2 }}>
        {composer.workspaceCommands().map((command) => (
          <li key={command.id}>
            <Typography component="span" variant="labelMedium">
              {command.name}
            </Typography>
            {` — ${command.description}`}
          </li>
        ))}
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

function YourPrompt({ text }: { text: string }): React.JSX.Element {
  return (
    <Box
      data-testid="your-prompt"
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

/** Opens the changes panel when it is hidden; the dot says it has something to show. */
export function PanelToggle({ count }: { count: number }): React.JSX.Element {
  const open = useDesktopLayout((state) => state.changesOpen);
  const label = open ? t('workspace.panel.hide', 'Hide panel') : t('workspace.panel.show', 'Show changes');
  return (
    <Tooltip title={`${label} (${modKey()}⌥\\)`}>
      <IconButton size="small" aria-label={label} aria-pressed={open} onClick={() => useDesktopLayout.getState().setChangesOpen(!open)}>
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
          : t('workspace.thread.newHint', 'Type @ to add a file, / for commands. The agent works in this folder, on this computer.')}
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

/** The header's way to the whole conversation in the Elitea chat. */
export function OpenInChat({ conversationId }: { conversationId: string }): React.JSX.Element {
  return (
    <Box sx={(theme: Theme) => ({ ...linkSx(theme), flexShrink: 0, whiteSpace: 'nowrap' })} data-testid="open-in-chat">
      <Link to="/chat/$conversationId" params={{ conversationId }}>
        {t('workspace.openInChat', 'Open in chat')}
      </Link>
    </Box>
  );
}

const quietSx = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

/** One recorded turn: its prompt, then its events replayed as the live transcript showed them. */
function RecordedTurn({ stored }: { stored: StoredTurn }): React.JSX.Element {
  const view = useMemo(() => replayView(stored.events), [stored.events]);
  return (
    <Box data-testid="history-turn" sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <YourPrompt text={stored.prompt} />
      <TurnTranscript view={view} busy={false} onCancel={() => undefined} />
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

function ServerTurn({ message }: { message: ServerMessage }): React.JSX.Element {
  if (message.role === 'user') return <YourPrompt text={message.content} />;
  return (
    // The chat's own renderer; raw HTML from the model is dropped, not rendered.
    <Markdown renderHtml={false} data-testid="history-answer">
      {message.content}
    </Markdown>
  );
}

/** The thread's earlier turns, above this session's: recorded on this computer, else the conversation's messages. */
export function EarlierTurns({ history, shown }: { history: ThreadHistory; shown: ReadonlySet<string> }): React.JSX.Element | null {
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
              <RecordedTurn key={stored.turn_id} stored={stored} />
            ))}
        </>
      );
    case 'server':
      return (
        <>
          {history.messages.map((message) => (
            <ServerTurn key={message.id} message={message} />
          ))}
        </>
      );
    case 'none':
    case 'unavailable':
      return null;
  }
}

/** Each turn started here, under the prompt that started it; the last one is live. ("Open in chat" is in the header.) */
export function Exchanges({ turn, prompts }: { turn: WorkspaceTurn; prompts: readonly string[] }): React.JSX.Element {
  const exchanges = [
    ...turn.earlier.map((entry, index) => ({ key: entry.turnId, prompt: prompts[index], view: entry.view, live: false })),
    ...(turn.turnId === null ? [] : [{ key: turn.turnId, prompt: prompts[turn.earlier.length], view: turn.view, live: true }]),
  ];
  return (
    <>
      {exchanges.map((exchange) => (
        <Box key={exchange.key} sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
          {exchange.prompt !== undefined && <YourPrompt text={exchange.prompt} />}
          <TurnTranscript view={exchange.view} busy={exchange.live && turn.busy} onCancel={() => void turn.cancel()} />
        </Box>
      ))}
    </>
  );
}

export function SessionNotices({ error, commands }: { error: string | null; commands: SessionCommands }): React.JSX.Element {
  return (
    <>
      {error !== null && <Alert severity="error">{error}</Alert>}
      {commands.notice === 'help' && <HelpNotice onClose={commands.closeNotice} />}
      {commands.notice === 'nothingToUndo' && (
        <Alert severity="info" onClose={commands.closeNotice}>
          {t('workspace.undoNothing', 'The last turn here changed no files.')}
        </Alert>
      )}
    </>
  );
}
