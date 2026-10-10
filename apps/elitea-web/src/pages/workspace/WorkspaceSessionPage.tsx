/**
 * Desktop-only `/workspaces/$workspaceId[?conversation=<id>]`: one THREAD of
 * a folder — the centre of the workspace-first window.
 *
 * Header: the folder, its project (re-binding moves the folder), the agent
 * and version. Body: the thread's exchanges — your prompt, then what the
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
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { ApprovalDialog, readThreads, useWorkspaceIpc } from '@/features/workspace';
import type { Workspace, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { ShowSidebarButton, TitleBarSpacer, useAppIpc, useDesktopLayout } from '@/widgets/desktop-shell';

import { AgentPickers, NoAgents } from './AgentPickers';
import { COLUMN, EarlierTurns, Exchanges, firstError, FolderTitle, OpenInChat, PanelToggle, SessionNotices, ThreadIntro } from './sessionParts';
import { SessionPanel } from './SessionPanel';
import { UnboundFolder } from './UnboundFolder';
import { useAgentSkills } from './useAgentSkills';
import { useStickToBottom } from './useStickToBottom';
import { useThreadSession, type ThreadSession, type ThreadSessionInput } from './useThreadSession';
import { WorkspaceComposer } from './WorkspaceComposer';

const LIST_KEY = ['workspace', 'list'] as const;

interface HeaderProps {
  session: ThreadSession;
  workspace: Workspace;
  projectId: number;
  conversationId: string;
  busy: boolean;
  projectMenuOpen: boolean;
  onProjectMenuOpenChange: (open: boolean) => void;
}

/** The folder, its project and agent, "Open in chat" for a started thread, and the panel toggle. */
function SessionHeader({ session, workspace, projectId, conversationId, busy, projectMenuOpen, onProjectMenuOpenChange }: HeaderProps): React.JSX.Element {
  const changesOpen = useDesktopLayout((state) => state.changesOpen);
  const sidebarOpen = useDesktopLayout((state) => state.sidebarOpen);
  const attention = session.turn.view.approvals.length + (session.undoTurnId === null ? 0 : 1);
  return (
    <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
      <TitleBarSpacer
        leading={!sidebarOpen}
        logo={false}
        trailing={
          <>
            {conversationId !== '' && <OpenInChat conversationId={conversationId} />}
            {!changesOpen && <PanelToggle count={attention} />}
          </>
        }
      >
        <ShowSidebarButton />
        <FolderTitle workspace={workspace} />
        <AgentPickers
          selection={session.selection}
          projectId={projectId}
          projects={session.projects}
          busy={busy}
          onChangeProject={(next) => session.rebind.mutate(next)}
          projectMenuOpen={projectMenuOpen}
          onProjectMenuOpenChange={onProjectMenuOpenChange}
          agentRef={session.commands.agentRef}
        />
      </TitleBarSpacer>
    </Box>
  );
}

function BoundSession(props: ThreadSessionInput): React.JSX.Element {
  const { ipc, workspace, projectId, conversationId } = props;
  const session = useThreadSession(props);
  const { turn, commands, raw } = session;
  const skills = useAgentSkills(projectId, raw.versionId);
  const [projectMenuOpen, setProjectMenuOpen] = useState(false);
  const appIpc = useAppIpc();
  const changesOpen = useDesktopLayout((state) => state.changesOpen);
  const [pendingApproval, ...waiting] = turn.view.approvals;
  const busy = turn.busy || session.rebind.isPending;
  const started = turn.turnId !== null || turn.earlier.length > 0;
  // The intro stands in for a thread with nothing to show: new, or its history unreadable.
  const nothingEarlier = session.history.kind === 'none' || session.history.kind === 'unavailable';
  const { scrollRef, contentRef } = useStickToBottom<HTMLDivElement, HTMLDivElement>();

  return (
    // Pinned to the frame's scroll box (which is `position: relative`): the
    // transcript scrolls in its own column and the composer stays below it.
    <Box data-testid="workspace-session" sx={{ position: 'absolute', inset: 0, display: 'flex', minHeight: 0 }}>
      <Box sx={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <SessionHeader
          session={session}
          workspace={workspace}
          projectId={projectId}
          conversationId={conversationId}
          busy={busy}
          projectMenuOpen={projectMenuOpen}
          onProjectMenuOpenChange={setProjectMenuOpen}
        />
        <Box ref={scrollRef} data-testid="session-transcript" sx={{ flex: 1, minHeight: 0, overflowY: 'auto' }}>
          <Box ref={contentRef} sx={{ ...COLUMN, paddingY: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
            {raw.empty && <NoAgents busy={busy} onCreate={session.createAgent} onOtherProject={() => setProjectMenuOpen(true)} />}
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
          </Box>
        </Box>
        <Box sx={{ ...COLUMN, flexShrink: 0, paddingTop: 1, paddingBottom: 2, display: 'flex', flexDirection: 'column', gap: 1 }}>
          <SessionNotices error={firstError(turn.startError, session.sendError, session.rebind.error)} commands={commands} />
          <WorkspaceComposer
            ipc={ipc}
            workspaceId={workspace.id}
            canSend={session.canSend}
            busy={turn.busy}
            planMode={commands.planMode}
            onPlanModeChange={commands.setPlanMode}
            skills={skills}
            onSend={(prompt, mentions, picked) => session.send(prompt, commands.planMode, mentions, picked)}
            onStop={() => void turn.cancel()}
            onCommand={session.run}
          />
        </Box>
      </Box>
      {changesOpen && (
        <SessionPanel
          ipc={ipc}
          appIpc={appIpc}
          workspaceId={workspace.id}
          changeSets={session.changeSets}
          undoTurnId={session.undoTurnId}
          undoRequest={commands.undoRequest}
          waiting={turn.view.approvals}
          decided={session.decided}
        />
      )}
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
