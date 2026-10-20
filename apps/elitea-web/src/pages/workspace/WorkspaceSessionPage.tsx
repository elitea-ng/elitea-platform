/**
 * Desktop-only `/workspaces/$workspaceId[?conversation=<id>]`: one THREAD of
 * a folder — the centre of the workspace-first window.
 *
 * Toolbar: the folder ▸ thread title (its path card re-binds the project).
 * Composer footer: agent·version, Act | Plan, where it runs. Body: the thread's exchanges — your prompt, then what the
 * agent did (text, collapsible tool lines). Bottom: the composer ("@" files,
 * "/" commands). Right (⌘⌥\): the changes panel (`SessionPanel`).
 *
 * The thread is the URL's `conversation`. A new thread has none; its first
 * send creates the conversation and the URL then names it (without
 * remounting, so the live turn stays on screen). Picking another thread in
 * the sidebar remounts the session: its own turn, subscription and picks.
 * Threads are remembered per folder on this machine (`features/workspace`
 * `threads.ts`) because a conversation does not know its folder.
 */
import { useState } from 'react';

import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { ApprovalDialog, readThreads, useWorkspaceIpc } from '@/features/workspace';
import { useHasIndexHost } from '@/features/workspace-index';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { useAppIpc, useDesktopLayout } from '@/widgets/desktop-shell';

import { NoAgents } from './AgentPickers';
import { CloudTranscript } from './CloudTranscript';
import { AgentChip, RunLocationChip } from './ComposerControls';
import { HandoffBanner, HandoffDialogs } from './HandoffControls';
import { COLUMN, EarlierTurns, Exchanges, firstError, SessionNotices, ThreadIntro } from './sessionParts';
import { SessionPanel } from './SessionPanel';
import { ThreadToolbar } from './ThreadToolbar';
import { UnboundFolder } from './UnboundFolder';
import { useAgentSkills } from './useAgentSkills';
import { useLocalCodingAgent } from './useLocalCodingAgent';
import { useStickToBottom } from './useStickToBottom';
import { useSessionMenuActions } from './useSessionMenuActions';
import { useThreadHandoff } from './useThreadHandoff';
import { useThreadSession, type ThreadSession, type ThreadSessionInput } from './useThreadSession';
import { WorkspaceComposer } from './WorkspaceComposer';

const LIST_KEY = ['workspace', 'list'] as const;

/** What the inspector's badge counts: approvals waiting, and a turn that can be undone. */
const attentionOf = (session: ThreadSession): number => session.turn.view.approvals.length + (session.undoTurnId === null ? 0 : 1);

const indexSettingsOpener = (hasIndex: boolean): (() => void) | undefined => (hasIndex ? () => useDesktopLayout.getState().showInspector('index') : undefined);

function BoundSession(props: ThreadSessionInput): React.JSX.Element {
  const { ipc, workspace, projectId, conversationId } = props;
  const session = useThreadSession(props);
  const { turn, commands, raw } = session;
  const skills = useAgentSkills(projectId, raw.versionId);
  const [projectMenuOpen, setProjectMenuOpen] = useState(false);
  const hasIndex = useHasIndexHost();
  const appIpc = useAppIpc();
  const changesOpen = useDesktopLayout((state) => state.changesOpen);
  // "Continue?" at the step limit is a card in the transcript; the rest are the dialog's queue.
  const continueRequest = turn.view.approvals.find((approval) => approval.kind === 'continue');
  const [pendingApproval, ...waiting] = turn.view.approvals.filter((approval) => approval.kind !== 'continue');
  const busy = turn.busy || session.rebind.isPending;
  const started = turn.turnId !== null || turn.earlier.length > 0;
  // The intro stands in for a thread with nothing to show: new, or its history unreadable.
  const nothingEarlier = session.history.kind === 'none' || session.history.kind === 'unavailable';
  const { scrollRef, contentRef } = useStickToBottom<HTMLDivElement, HTMLDivElement>();
  const handoff = useThreadHandoff(workspace.id, projectId, conversationId);
  const codingAgent = useLocalCodingAgent(projectId, raw.empty);

  useSessionMenuActions({ handoff, conversationId, workspace, hasIndex, agentRef: commands.agentRef, openIndexSettings: () => useDesktopLayout.getState().showInspector('index') });

  return (
    // Pinned to the frame's scroll box (which is `position: relative`): the
    // transcript scrolls in its own column and the composer stays below it.
    <Box data-testid="workspace-session" sx={{ position: 'absolute', inset: 0, display: 'flex', minHeight: 0 }}>
      <Box sx={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <ThreadToolbar
          workspace={workspace}
          conversationId={conversationId}
          projectId={projectId}
          projects={session.projects}
          busy={busy}
          onChangeProject={(next) => session.rebind.mutate(next)}
          pathOpen={projectMenuOpen}
          onPathOpenChange={setProjectMenuOpen}
          attention={attentionOf(session)}
          onIndexSettings={indexSettingsOpener(hasIndex)}
        />
        <Box ref={scrollRef} data-testid="session-transcript" sx={{ flex: 1, minHeight: 0, overflowY: 'auto' }}>
          <Box ref={contentRef} sx={{ ...COLUMN, paddingY: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
            {handoff.offLaptop ? (
              <CloudTranscript projectId={projectId} conversationId={conversationId} />
            ) : (
              <>
              {raw.empty && (
                <NoAgents codingAgent={codingAgent} busy={busy} onCreate={session.createAgent} onOtherProject={() => setProjectMenuOpen(true)} />
              )}
              {!raw.empty && !started && nothingEarlier && (
                <ThreadIntro
                  workspace={workspace}
                  conversationId={conversationId}
                  unavailable={session.history.kind === 'unavailable'}
                  title={readThreads(workspace.id).find((x) => x.id === conversationId)?.title}
                />
              )}
              <EarlierTurns history={session.history} shown={session.shown} />
              <Exchanges turn={turn} prompts={session.prompts} />
              <ApprovalDialog request={continueRequest} queued={0} onRespond={session.answer} />
              </>
            )}
          </Box>
        </Box>
        <Box sx={{ ...COLUMN, flexShrink: 0, paddingTop: 1, paddingBottom: 2, display: 'flex', flexDirection: 'column', gap: 1 }}>
          <HandoffBanner handoff={handoff} />
          <SessionNotices error={firstError(turn.startError, session.sendError, session.rebind.error)} commands={commands} />
          <WorkspaceComposer
            ipc={ipc}
            workspaceId={workspace.id}
            canSend={session.canSend && !handoff.offLaptop}
            readOnly={handoff.offLaptop}
            busy={turn.busy}
            planMode={commands.planMode}
            agentPicker={<AgentChip selection={session.selection} agentRef={commands.agentRef} />}
            runLocation={<RunLocationChip handoff={handoff} />}
            onPlanModeChange={commands.setPlanMode}
            skills={skills}
            onSend={(prompt, mentions, picked) => session.send(prompt, commands.planMode, mentions, picked)}
            onStop={() => void turn.cancel()}
            onCommand={session.run}
            onFocus={session.prewarm}
          />
        </Box>
      </Box>
      {changesOpen && (
        <SessionPanel
          ipc={ipc}
          appIpc={appIpc}
          workspaceId={workspace.id}
          hasIndex={hasIndex}
          changeSets={session.changeSets}
          undoTurnId={session.undoTurnId}
          undoRequest={commands.undoRequest}
          waiting={turn.view.approvals}
          decided={session.decided}
        />
      )}
      <HandoffDialogs handoff={handoff} workspaceId={workspace.id} projectId={projectId} />
      <ApprovalDialog request={pendingApproval} queued={waiting.length} onRespond={session.answer} />
    </Box>
  );
}

function Session({ ipc, workspaceId, conversationId }: { ipc: WorkspaceIpc; workspaceId: string; conversationId: string }): React.JSX.Element {
  const list = useQuery({ queryKey: LIST_KEY, queryFn: () => ipc.list() });
  // The session's key moves with the thread, except when the URL catches up
  // with a conversation the session itself just created (`adopted`).
  const [thread, setThread] = useState({ url: conversationId, key: 0, adopted: null as string | null });
  if (conversationId !== thread.url) {
    setThread(
      conversationId === thread.adopted
        ? { url: conversationId, key: thread.key, adopted: null }
        : { url: conversationId, key: thread.key + 1, adopted: null },
    );
  }
  const navigate = useNavigate();
  const adopt = (id: string): void => {
    setThread((current) => ({ ...current, adopted: id }));
    void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId }, search: { conversation: id }, replace: true });
  };

  const workspace = list.data?.find((w) => w.id === workspaceId);
  if (list.isPending) return <Typography sx={{ padding: 3 }}>{t('workspace.loading', 'Loading workspaces')}</Typography>;
  if (workspace === undefined) {
    return (
      <Box sx={{ padding: 3 }}>
        <Alert severity="warning">{t('workspace.notFound', 'This folder is no longer in your workspaces.')}</Alert>
      </Box>
    );
  }
  if (workspace.project_id === null) return <UnboundFolder ipc={ipc} workspace={workspace} />;
  // Keyed: another folder, another project bound to it, or another thread is
  // a new session — its own turn, event subscription and agent pick.
  return (
    <BoundSession
      key={`${workspace.id}:${String(workspace.project_id)}:${String(thread.key)}`}
      ipc={ipc}
      workspace={workspace}
      projectId={workspace.project_id}
      conversationId={conversationId}
      onAdopt={adopt}
    />
  );
}

export default function WorkspaceSessionPage(): React.JSX.Element {
  const { workspaceId } = useParams({ strict: false }) as { workspaceId?: string };
  const search = useSearch({ strict: false }) as { conversation?: unknown };
  const conversationId = typeof search.conversation === 'string' || typeof search.conversation === 'number' ? String(search.conversation) : '';
  const ipc = useWorkspaceIpc();
  if (ipc === undefined || workspaceId === undefined) {
    return (
      <Box sx={{ padding: 3 }}>
        <Alert severity="info">{t('workspace.noHost', 'Workspaces need the Elitea desktop app.')}</Alert>
      </Box>
    );
  }
  return <Session ipc={ipc} workspaceId={workspaceId} conversationId={conversationId} />;
}
