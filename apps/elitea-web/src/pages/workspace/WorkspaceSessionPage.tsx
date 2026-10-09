/**
 * Desktop-only `/workspaces/$workspaceId`: pick an agent from the workspace's
 * bound project, write a prompt, watch the turn run on this machine, approve
 * what it asks to do, and review/undo what it changed.
 *
 * Agent and version selection reuse the app's own application queries (the
 * same generated hooks the agents page uses); the chat composer's version bar
 * is tied to the chat form, so plain selects stand in for it here.
 */
import { useState, type SubmitEvent } from 'react';

import { Link, useParams } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { ApprovalDialog, ChangedFilesCard, TurnTranscript, useWorkspaceIpc, useWorkspaceTurn } from '@/features/workspace';
import type { Workspace, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { AgentPickers } from './AgentPickers';
import { useAgentSelection } from './useAgentSelection';
import { useSendPrompt } from './useSendPrompt';

function Composer({ canSend, onSend }: { canSend: boolean; onSend: (prompt: string, planMode: boolean) => Promise<boolean> }): React.JSX.Element {
  const [prompt, setPrompt] = useState('');
  const [planMode, setPlanMode] = useState(false);
  const submit = (event: SubmitEvent<HTMLFormElement>): void => {
    event.preventDefault();
    void onSend(prompt, planMode).then((sent) => {
      if (sent) setPrompt('');
    });
  };
  return (
    <Box component="form" onSubmit={submit} sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <TextField multiline minRows={3} label={t('workspace.prompt', 'What should the agent do?')} value={prompt} onChange={(e) => setPrompt(e.target.value)} />
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 2 }}>
        <FormControlLabel
          control={<Switch checked={planMode} onChange={(e) => setPlanMode(e.target.checked)} />}
          label={t('workspace.planMode', 'Plan mode (no changes)')}
        />
        <Button type="submit" variant="contained" disabled={!canSend || prompt.trim() === ''} sx={{ marginLeft: 'auto' }}>
          {t('workspace.send', 'Send')}
        </Button>
      </Box>
    </Box>
  );
}

function BoundSession({ ipc, workspace, projectId }: { ipc: WorkspaceIpc; workspace: Workspace; projectId: number }): React.JSX.Element {
  const selection = useAgentSelection(projectId);
  const turn = useWorkspaceTurn(ipc);
  const { canSend, sendError, send } = useSendPrompt(workspace, projectId, selection, turn);
  const [pendingApproval, ...waiting] = turn.view.approvals;
  const done = turn.view.done;

  return (
    <Box data-testid="workspace-session" sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Box>
        <Typography component="h1" variant="headingMedium">
          {workspace.name}
        </Typography>
        <Typography variant="bodySmall">{workspace.path}</Typography>
      </Box>
      <AgentPickers selection={selection} />
      <TurnTranscript view={turn.view} busy={turn.busy} onCancel={() => void turn.cancel()} />
      {(turn.startError ?? sendError) !== null && <Alert severity="error">{turn.startError ?? sendError}</Alert>}
      {done !== undefined && turn.turnId !== null && done.changedFiles > 0 && <ChangedFilesCard ipc={ipc} turnId={turn.turnId} />}
      {done?.committed === true && (
        <Link to="/chat/$conversationId" params={{ conversationId: done.conversationId }}>
          {t('workspace.openInChat', 'Open in chat')}
        </Link>
      )}
      <Composer canSend={canSend} onSend={send} />
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
