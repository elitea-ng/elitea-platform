/**
 * Desktop-only `/workspaces/$workspaceId`: pick an agent from the workspace's
 * bound project, write a prompt, watch the turn run on this machine, approve
 * what it asks to do, and review/undo what it changed.
 *
 * Agent and version selection reuse the app's own application queries (the
 * same generated hooks the agents page uses); the chat composer's version bar
 * is tied to the chat form, so plain selects stand in for it here. The
 * composer itself is the chat page's input, with "@" files and "/" commands
 * (`WorkspaceComposer`).
 */
import { Link, useNavigate, useParams } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import {
  ApprovalDialog,
  ChangedFilesCard,
  composer,
  describeWorkspaceError,
  TurnTranscript,
  useWorkspaceIpc,
  useWorkspaceTurn,
} from '@/features/workspace';
import type { Workspace, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { useSelectedProject } from '@/widgets/app-shell';

import { AgentPickers } from './AgentPickers';
import { useAgentSelection } from './useAgentSelection';
import { useBindableProjects } from './useBindableProjects';
import { useSendPrompt } from './useSendPrompt';
import { useSessionCommands } from './useSessionCommands';
import { WorkspaceComposer } from './WorkspaceComposer';

const LIST_KEY = ['workspace', 'list'] as const;

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
function firstError(startError: string | null, sendError: string | null, rebindError: unknown): string | null {
  if (startError !== null) return startError;
  if (sendError !== null) return sendError;
  return rebindError === null || rebindError === undefined ? null : describeWorkspaceError(rebindError);
}

function BoundSession({ ipc, workspace, projectId }: { ipc: WorkspaceIpc; workspace: Workspace; projectId: number }): React.JSX.Element {
  const selection = useAgentSelection(workspace.id, projectId);
  const turn = useWorkspaceTurn(ipc);
  const { canSend, sendError, send } = useSendPrompt(workspace, projectId, selection, turn);
  const projects = useBindableProjects();
  const { selectProject } = useSelectedProject();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [pendingApproval, ...waiting] = turn.view.approvals;
  const done = turn.view.done;
  const undoTurnId = done !== undefined && done.changedFiles > 0 ? turn.turnId : null;
  const commands = useSessionCommands(selection, turn, undoTurnId !== null);

  // Re-binding re-keys the session (see `Session`): the new project's agents, a fresh turn.
  const rebind = useMutation({
    mutationFn: (next: number) => ipc.bindProject(workspace.id, next),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: LIST_KEY }),
  });

  const createAgent = (): void => {
    // The agent editor creates in the app's selected project: make it the bound one first.
    const bound = projects.find((p) => p.id === projectId);
    if (bound !== undefined) selectProject(String(bound.id), bound.name);
    void navigate({ to: '/agents/create' });
  };

  const error = firstError(turn.startError, sendError, rebind.error);

  return (
    <Box data-testid="workspace-session" sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Box>
        <Typography component="h1" variant="headingMedium">
          {workspace.name}
        </Typography>
        <Typography variant="bodySmall">{workspace.path}</Typography>
      </Box>
      <AgentPickers
        selection={selection}
        projectId={projectId}
        projects={projects}
        busy={turn.busy || rebind.isPending}
        onChangeProject={(next) => rebind.mutate(next)}
        onCreateAgent={createAgent}
        agentRef={commands.agentRef}
      />
      <TurnTranscript view={turn.view} busy={turn.busy} onCancel={() => void turn.cancel()} />
      {error !== null && <Alert severity="error">{error}</Alert>}
      {undoTurnId !== null && <ChangedFilesCard ipc={ipc} turnId={undoTurnId} undoRequest={commands.undoRequest} />}
      {done?.committed === true && (
        <Link to="/chat/$conversationId" params={{ conversationId: done.conversationId }}>
          {t('workspace.openInChat', 'Open in chat')}
        </Link>
      )}
      {commands.notice === 'help' && <HelpNotice onClose={commands.closeNotice} />}
      {commands.notice === 'nothingToUndo' && (
        <Alert severity="info" onClose={commands.closeNotice}>
          {t('workspace.undoNothing', 'The last turn here changed no files.')}
        </Alert>
      )}
      <WorkspaceComposer
        ipc={ipc}
        workspaceId={workspace.id}
        canSend={canSend}
        busy={turn.busy}
        planMode={commands.planMode}
        onPlanModeChange={commands.setPlanMode}
        onSend={(prompt, mentions) => send(prompt, commands.planMode, mentions)}
        onStop={() => void turn.cancel()}
        onCommand={commands.run}
      />
      <ApprovalDialog request={pendingApproval} queued={waiting.length} onRespond={(id, decision) => void turn.answer(id, decision)} />
    </Box>
  );
}

function Session({ ipc, workspaceId }: { ipc: WorkspaceIpc; workspaceId: string }): React.JSX.Element {
  const list = useQuery({ queryKey: ['workspace', 'list'], queryFn: () => ipc.list() });
  const workspace = list.data?.find((w) => w.id === workspaceId);
  if (list.isPending) return <Typography>{t('workspace.loading', 'Loading workspaces')}</Typography>;
  if (workspace === undefined) return <Alert severity="warning">{t('workspace.notFound', 'This folder is no longer in your workspaces.')}</Alert>;
  if (workspace.project_id === null) return <Alert severity="info">{t('workspace.needsProject', 'Bind a project to this folder first.')}</Alert>;
  // Keyed: another folder (or another project bound to it) is a new session —
  // its own turn, event subscription, agent pick and pending conversation.
  return <BoundSession key={`${workspace.id}:${String(workspace.project_id)}`} ipc={ipc} workspace={workspace} projectId={workspace.project_id} />;
}

export default function WorkspaceSessionPage(): React.JSX.Element {
  const { workspaceId } = useParams({ strict: false }) as { workspaceId?: string };
  const ipc = useWorkspaceIpc();
  if (ipc === undefined || workspaceId === undefined) {
    return (
      <Box sx={{ p: '1.5rem' }}>
        <Alert severity="info">{t('workspace.noHost', 'Workspaces need the Elitea desktop app.')}</Alert>
      </Box>
    );
  }
  return (
    <Box sx={{ p: '1.5rem' }}>
      <Session ipc={ipc} workspaceId={workspaceId} />
    </Box>
  );
}
