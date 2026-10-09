/**
 * The state of one thread on screen: the agent pick, the live turn, the
 * prompts sent here (the transcript pairs each with its turn), approvals
 * decided, and the side effects that tie the thread to the shell — the URL,
 * the folder's thread list, the last location, the app's project, and the
 * actions ⌘N / the palette reach.
 */
import { useEffect, useRef, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import {
  readThreads,
  recordThread,
  threadsQueryKey,
  useWorkspaceTurn,
  writeLastLocation,
  type WorkspaceCommandId,
  type WorkspaceTurn,
} from '@/features/workspace';
import type { ApprovalDecision, Workspace, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { useSelectedProject } from '@/widgets/app-shell';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import type { DecidedApproval } from './SessionPanel';
import { useAgentSelection, type AgentSelection } from './useAgentSelection';
import { useBindableProjects } from './useBindableProjects';
import { useSendPrompt, type SendPrompt } from './useSendPrompt';
import { useSessionCommands, type SessionCommands } from './useSessionCommands';

const LIST_KEY = ['workspace', 'list'] as const;

export interface ThreadSessionInput {
  ipc: WorkspaceIpc;
  workspace: Workspace;
  projectId: number;
  /** The thread the URL opened ('' = a new thread). */
  conversationId: string;
  /** The first send of a new thread created `id`: the page puts it in the URL without remounting. */
  onAdopt: (id: string) => void;
}

/** The selection the header and the "/" commands drive: leaving for a new thread goes through the URL. */
function threadAware(selection: AgentSelection, leaveThread: () => void): AgentSelection {
  return {
    ...selection,
    selectConversation: (id) => {
      if (id === '') leaveThread();
      else selection.selectConversation(id);
    },
  };
}

/** The bound project becomes the app's selected one: the Elitea pages follow the folder. */
function useFolderProject(projectId: number): ReturnType<typeof useBindableProjects> {
  const projects = useBindableProjects();
  const { project: selected, selectProject } = useSelectedProject();
  const selectedId = selected?.id;
  useEffect(() => {
    const bound = projects.find((p) => p.id === projectId);
    if (bound !== undefined && selectedId !== String(bound.id)) selectProject(String(bound.id), bound.name);
  }, [projects, projectId, selectedId, selectProject]);
  return projects;
}

/** What the shell's ⌘N / palette reach while this thread is on screen. */
function useShellRegistration(workspaceId: string, newThread: () => void, run: (command: WorkspaceCommandId) => void): void {
  const latest = useRef({ newThread, run });
  useEffect(() => {
    latest.current = { newThread, run };
  });
  useEffect(() => {
    useDesktopLayout.getState().setSession({
      workspaceId,
      newThread: () => latest.current.newThread(),
      togglePlanMode: () => latest.current.run('plan'),
    });
    return () => useDesktopLayout.getState().setSession(null);
  }, [workspaceId]);
}

export interface ThreadSession extends Pick<SendPrompt, 'canSend' | 'sendError' | 'send'> {
  raw: AgentSelection;
  selection: AgentSelection;
  turn: WorkspaceTurn;
  commands: SessionCommands;
  run: (command: WorkspaceCommandId) => void;
  prompts: readonly string[];
  decided: readonly DecidedApproval[];
  answer: (requestId: string, decision: ApprovalDecision) => void;
  undoTurnId: string | null;
  projects: ReturnType<typeof useBindableProjects>;
  rebind: UseMutationResult<void, Error, number>;
  createAgent: () => void;
}

export function useThreadSession({ ipc, workspace, projectId, conversationId, onAdopt }: ThreadSessionInput): ThreadSession {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const raw = useAgentSelection(workspace.id, projectId);
  const turn = useWorkspaceTurn(ipc);
  const [prompts, setPrompts] = useState<string[]>([]);
  const [decided, setDecided] = useState<DecidedApproval[]>([]);
  const projects = useFolderProject(projectId);
  const { selectProject } = useSelectedProject();

  // The URL's thread is the conversation (adjusted during render, once per mount).
  const [applied, setApplied] = useState(false);
  if (!applied) {
    setApplied(true);
    if (conversationId !== '') raw.selectConversation(conversationId);
  }

  const newThread = (): void => void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId: workspace.id }, search: {} });
  const selection = threadAware(raw, newThread);

  const onStarted = (id: string, prompt: string): void => {
    setPrompts((current) => [...current, prompt]);
    const known = readThreads(workspace.id).find((thread) => thread.id === id);
    recordThread(workspace.id, { id, title: known?.title ?? prompt, updatedAt: Date.now() });
    void queryClient.invalidateQueries({ queryKey: threadsQueryKey(workspace.id) });
    if (id !== conversationId) onAdopt(id);
  };
  const { canSend, sendError, send } = useSendPrompt(workspace, projectId, raw, turn, onStarted);
  const done = turn.view.done;
  const undoTurnId = done !== undefined && done.changedFiles > 0 ? turn.turnId : null;
  const commands = useSessionCommands(selection, turn, undoTurnId !== null);

  const run = (command: WorkspaceCommandId): void => {
    if ((command === 'clear' || command === 'new') && !turn.busy) setPrompts([]);
    commands.run(command);
  };
  useShellRegistration(workspace.id, newThread, run);

  // Where to come back to after a restart.
  useEffect(() => writeLastLocation({ workspaceId: workspace.id, conversationId }), [workspace.id, conversationId]);

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

  const answer = (requestId: string, decision: ApprovalDecision): void => {
    const request = turn.view.approvals.find((a) => a.request_id === requestId);
    if (request !== undefined) setDecided((current) => [...current, { requestId, title: request.title, decision }]);
    void turn.answer(requestId, decision);
  };

  return { raw, selection, turn, commands, run, prompts, decided, answer, undoTurnId, projects, rebind, createAgent, canSend, sendError, send };
}
