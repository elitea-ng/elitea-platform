/**
 * The pieces of a thread screen (`WorkspaceSessionPage`): the folder title,
 * the panel toggle, your prompt, the empty thread's intro, the exchanges,
 * and the notices above the composer.
 */
import { Link } from '@tanstack/react-router';

import VerticalSplitOutlinedIcon from '@mui/icons-material/VerticalSplitOutlined';
import Alert from '@mui/material/Alert';
import Badge from '@mui/material/Badge';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { composer, describeWorkspaceError, TurnTranscript, type WorkspaceTurn } from '@/features/workspace';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { modKey, useDesktopLayout } from '@/widgets/desktop-shell';

import type { SessionCommands } from './useSessionCommands';

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
        background: theme.vars.palette.background.secondary,
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
    <Box sx={{ minWidth: 0, display: 'flex', flexDirection: 'column' }}>
      <Typography component="h1" variant="labelMedium" sx={{ margin: 0, whiteSpace: 'nowrap' }}>
        {workspace.name}
      </Typography>
      <Typography
        variant="bodySmall"
        title={workspace.path}
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', direction: 'rtl', textAlign: 'left' })}
      >
        {workspace.path}
      </Typography>
    </Box>
  );
}

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

export function ThreadIntro({ workspace, conversationId, title }: { workspace: Workspace; conversationId: string; title: string | undefined }): React.JSX.Element {
  const isNew = conversationId === '';
  return (
    <Box sx={{ paddingY: 6, textAlign: 'center', display: 'flex', flexDirection: 'column', gap: 1, alignItems: 'center' }}>
      <Typography variant="headingMedium" component="p">
        {isNew ? t('workspace.thread.newTitle', 'What should we do in {{name}}?', { name: workspace.name }) : (title ?? t('workspace.thread.continue', 'Continue this thread'))}
      </Typography>
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {isNew
          ? t('workspace.thread.newHint', 'Type @ to add a file, / for commands. The agent works in this folder, on this computer.')
          : t('workspace.thread.earlierHint', 'Earlier messages of this thread are in the conversation.')}
      </Typography>
      {!isNew && (
        <Link to="/chat/$conversationId" params={{ conversationId }}>
          {t('workspace.openInChat', 'Open in chat')}
        </Link>
      )}
    </Box>
  );
}

/** Each turn started here, under the prompt that started it; the last one is live. */
export function Exchanges({ turn, prompts }: { turn: WorkspaceTurn; prompts: readonly string[] }): React.JSX.Element {
  const exchanges = [
    ...turn.earlier.map((entry, index) => ({ key: entry.turnId, prompt: prompts[index], view: entry.view, live: false })),
    ...(turn.turnId === null ? [] : [{ key: turn.turnId, prompt: prompts[turn.earlier.length], view: turn.view, live: true }]),
  ];
  const done = turn.view.done?.committed === true ? turn.view.done : undefined;
  return (
    <>
      {exchanges.map((exchange) => (
        <Box key={exchange.key} sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
          {exchange.prompt !== undefined && <YourPrompt text={exchange.prompt} />}
          <TurnTranscript view={exchange.view} busy={exchange.live && turn.busy} onCancel={() => void turn.cancel()} />
        </Box>
      ))}
      {done !== undefined && (
        <Link to="/chat/$conversationId" params={{ conversationId: done.conversationId }}>
          {t('workspace.openInChat', 'Open in chat')}
        </Link>
      )}
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
