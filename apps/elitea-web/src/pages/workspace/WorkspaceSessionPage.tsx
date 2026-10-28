/**
 * Desktop-only `/workspaces/$workspaceId[?conversation=<id>]`: one THREAD of
 * a folder — the centre of the workspace-first window.
 *
 * Toolbar: the folder ▸ thread title (its path card re-binds the project).
 * Composer footer: agent·version, Act | Plan, where it runs. Body: the thread's exchanges — your prompt, then what the
 * agent did (text, collapsible tool lines). Bottom: the composer ("@" files,
 * "/" commands). Right (⌘⌥\): the thread's tabs in the frame's inspector —
 * Changes, Approvals, Index (`SessionPanel.tsx`).
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
import { useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { approval, plan, readThreads, threadsQueryKey, useWorkspaceIpc, type WorkspaceTurn } from '@/features/workspace';
import { HooksProposals } from '@/features/workspace-hooks';
import { useHasIndexHost } from '@/features/workspace-index';
import { McpProposals } from '@/features/workspace-mcp';
import { getListApplicationsQueryKey } from '@/shared/api/generated/applications/applications';
import type { Workspace, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { useAppIpc, useDesktopLayout } from '@/widgets/desktop-shell';

import { NoAgents } from './AgentPickers';
import { CloudTranscript } from './CloudTranscript';
import { CreateAgentDialog } from './CreateAgentDialog';
import { HandoffDialogs } from './HandoffControls';
import { ForkNotices } from './forkNotices';
import { CONVERSATION_COLUMN as COLUMN } from '@/shared/ui/conversation-column';

import { EarlierTurns, Exchanges, ThreadIntro } from './sessionParts';
import { SessionInspector } from './SessionPanel';
import { ThreadComposer } from './ThreadComposer';
import { ThreadToolbar } from './ThreadToolbar';
import { UnboundFolder } from './UnboundFolder';
import { threadMeter } from './contextBreakdown';
import { useIdleCompacted, withIdleCompaction, type IdleCompacted } from './useIdleCompacted';
import { useCloudChat } from './useCloudChat';
import { useSessionFork } from './useForkThread';
import { useAgentSkills } from './useAgentSkills';
import { useLocalCodingAgent } from './useLocalCodingAgent';
import { useStickToBottom } from './useStickToBottom';
import { useSessionMenuActions } from './useSessionMenuActions';
import { useThreadHandoff, type ThreadHandoff } from './useThreadHandoff';
import type { ThreadHistory } from './useThreadHistory';
import { useThreadSession, type ThreadSession, type ThreadSessionInput } from './useThreadSession';

const LIST_KEY = ['workspace', 'list'] as const;

/** What the inspector's badge counts: approvals waiting, and a turn that can be undone. */
const attentionOf = (session: ThreadSession): number =>
  session.turn.view.approvals.length + (session.turn.view.planProposal === undefined ? 0 : 1) + (session.undoTurnId === null ? 0 : 1);

/**
 * What the thread's inspector tabs show: the thread's title, or its folder's
 * name (a new thread). Read through the sidebar's threads query (the same
 * key, invalidated after `recordThread`), not from storage on every render.
 */
function useThreadSubject(workspace: Workspace, conversationId: string): string {
  const threads = useQuery({ queryKey: threadsQueryKey(workspace.id), queryFn: () => readThreads(workspace.id) });
  return threads.data?.find((x) => x.id === conversationId)?.title ?? workspace.name;
}

/** The Context tab's figures, after a background idle compaction: none for the cloud copy (its context is the cloud's). */
const inspectorMeter = (handoff: ThreadHandoff, turn: WorkspaceTurn, history: ThreadHistory, compacted: IdleCompacted | null) =>
  handoff.status?.executor === 'cloud' ? undefined : withIdleCompaction(threadMeter(turn, history), compacted);

const indexSettingsOpener = (hasIndex: boolean): (() => void) | undefined => (hasIndex ? () => useDesktopLayout.getState().showInspector('index') : undefined);

function BoundSession(props: ThreadSessionInput): React.JSX.Element {
  const { ipc, workspace, projectId, conversationId } = props;
  const session = useThreadSession(props);
  const subject = useThreadSubject(workspace, conversationId);
  const idleCompacted = useIdleCompacted(workspace.id, conversationId);
  const { turn, commands, raw } = session;
  const skills = useAgentSkills(projectId, raw.versionId);
  const hasIndex = useHasIndexHost();
  const appIpc = useAppIpc();
  // "Continue?" at the step limit and an MCP sign-in are cards in the transcript; the rest are the dialog's queue.
  const inline = (kind: string | undefined): boolean => kind === 'continue' || kind === 'mcp_authorization';
  const continueRequest = turn.view.approvals.find((approval) => inline(approval.kind));
  const [pendingApproval, ...waiting] = turn.view.approvals.filter((approval) => !inline(approval.kind));
  const busy = turn.busy || session.rebind.isPending;
  const started = turn.turnId !== null || turn.earlier.length > 0;
  // The intro stands in for a thread with nothing to show: new, or its history unreadable.
  const nothingEarlier = session.history.kind === 'none' || session.history.kind === 'unavailable';
  const { scrollRef, contentRef } = useStickToBottom<HTMLDivElement, HTMLDivElement>();
  const handoff = useThreadHandoff(workspace.id, projectId, conversationId);
  const codingAgent = useLocalCodingAgent(projectId, raw.empty);
  // A thread away from this computer is chatted with on the server (the cloud copy); `cloud` is idle otherwise.
  const cloud = useCloudChat({ conversationId, workspace, active: handoff.offLaptop, mayChat: handoff.status?.executor === 'cloud' });

  useSessionMenuActions({ handoff, conversationId, workspace, hasIndex, agentRef: commands.agentRef, openIndexSettings: () => useDesktopLayout.getState().showInspector('index'), createAgent: () => setCreateAgentOpen(true) });
  const forking = useSessionFork({ ipc, workspaceId: workspace.id, projectId, conversationId, offLaptop: handoff.offLaptop });
  const queryClient = useQueryClient();
  // ⋯ ▸ Create Agent… and the palette's "Create agent…" (ADR-0037).
  const [createAgentOpen, setCreateAgentOpen] = useState(false);
  const openCreateAgent = (): void => setCreateAgentOpen(true);

  return (
    // Pinned to the frame's scroll box (which is `position: relative`): the
    // transcript scrolls in its own column and the composer stays below it.
    <Box data-testid="workspace-session" sx={{ position: 'absolute', inset: 0, display: 'flex', minHeight: 0 }}>
      <Box sx={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <ThreadToolbar
          workspace={workspace}
          conversationId={conversationId}
          projectId={projectId}
          busy={busy}
          attention={attentionOf(session)}
          onIndexSettings={indexSettingsOpener(hasIndex)}
          onFork={forking.onForkThread}
          onCreateAgent={openCreateAgent}
        />
        <Box ref={scrollRef} data-testid="session-transcript" sx={{ flex: 1, minHeight: 0, overflowY: 'auto' }}>
          <Box ref={contentRef} sx={{ ...COLUMN, paddingY: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
            {handoff.offLaptop ? (
              <CloudTranscript chat={cloud} />
            ) : (
              <>
              {raw.empty && (
                <NoAgents
                  codingAgent={codingAgent}
                  busy={busy}
                  onCreate={session.createAgent}
                  otherProjects={session.projects.filter((project) => project.id !== projectId)}
                  onMoveTo={(next) => session.rebind.mutate(next)}
                />
              )}
              {!raw.empty && !started && nothingEarlier && (
                <ThreadIntro
                  workspace={workspace}
                  conversationId={conversationId}
                  unavailable={session.history.kind === 'unavailable'}
                  title={readThreads(workspace.id).find((x) => x.id === conversationId)?.title}
                />
              )}
              <ForkNotices fork={forking} ipc={ipc} />
              <McpProposals workspaceId={workspace.id} />
              <HooksProposals workspaceId={workspace.id} />
              <EarlierTurns history={session.history} shown={session.shown} onFork={forking.onFork} />
              <Exchanges turn={turn} prompts={session.prompts} promptImages={session.promptImages} onFork={forking.onFork} live={{ onRetryTurn: () => session.run('retry'), onContinue: commands.continueTurn }} />
              <approval.Dialog request={continueRequest} queued={0} onRespond={session.answer} />
              <plan.Card proposal={turn.view.planProposal} onRespond={session.answer} />
              </>
            )}
          </Box>
        </Box>
        <Box sx={{ ...COLUMN, flexShrink: 0, paddingTop: 1, paddingBottom: 2, display: 'flex', flexDirection: 'column', gap: 1 }}>
          <ThreadComposer ipc={ipc} workspaceId={workspace.id} projectId={projectId} conversationId={conversationId} session={session} turn={turn} handoff={handoff} cloud={cloud} skills={skills} canBranch={workspace.is_git && workspace.parent_id === undefined} />
        </Box>
      </Box>
      <SessionInspector
        ipc={ipc}
        appIpc={appIpc}
        workspaceId={workspace.id}
        subject={subject}
        hasIndex={hasIndex}
        changeSets={session.changeSets}
        undoTurnId={session.undoTurnId}
        undoRequest={commands.undoRequest}
        waiting={turn.view.approvals}
        decided={session.decided}
        plan={session.plan}
        planMode={commands.planMode}
        context={inspectorMeter(handoff, turn, session.history, idleCompacted)}
      />
      <HandoffDialogs handoff={handoff} workspaceId={workspace.id} projectId={projectId} />
      <CreateAgentDialog
        open={createAgentOpen}
        onClose={() => setCreateAgentOpen(false)}
        projectId={projectId}
        localFolder={!handoff.offLaptop}
        canSendLocal={session.canSend}
        onLocal={(prompt) => void session.send(prompt, 'act', [])}
        onCreated={(result) => {
          // The new agent lists in this session at once and is picked: the next message runs it.
          void queryClient.invalidateQueries({ queryKey: getListApplicationsQueryKey(String(projectId)) });
          session.selection.selectAgent(String(result.id));
        }}
      />
      <approval.Dialog request={pendingApproval} queued={waiting.length} onRespond={session.answer} />
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
