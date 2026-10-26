/**
 * "Send": make sure the conversation exists, then start the turn on the host.
 */
import { useRef, useState } from 'react';

import { describeWorkspaceError, type WorkspaceTurn } from '@/features/workspace';
import type { TurnModelChoice } from '@/shared/desktop/turnModel';
import type { AttachedImage, TurnCommand, TurnImagePick, TurnMode, TurnStartRequest, Workspace } from '@/shared/desktop/workspaceIpc';

import type { AgentSelection } from './useAgentSelection';
import { useEnsureConversation } from './useEnsureConversation';

/** The images a message carries: the tokens the host holds them by, and what the transcript shows of them. */
export interface SentImages {
  picks: TurnImagePick[];
  shown: readonly Pick<AttachedImage, 'name' | 'thumbnail'>[];
}

export interface SendPrompt {
  canSend: boolean;
  sendError: string | null;
  /** `mentions`: the workspace paths the prompt references with "@"; `skills`: the skills it invokes with "/"; `compact`: `/compact` was asked for; `model`: the model chip's choice; `command`: the custom command it runs. */
  send: (prompt: string, mode: TurnMode, mentions: string[], skills?: TurnStartRequest['skills'], compact?: boolean, model?: TurnModelChoice, command?: TurnCommand, images?: SentImages) => Promise<boolean>;
  /** The composer got focus with an agent selected: the host reads ahead what the send will need. */
  prewarm: () => void;
}

/**
 * A conversation created for a send that did not start (the participants add
 * failed, or the host refused the start), and the agent version it was
 * created for. `ready`: its participants were added.
 */
interface PendingConversation {
  key: string;
  id: string;
  ready: boolean;
}

/** The pending conversation a "new conversation" send for `key` continues in, if any. */
function reusable(pending: PendingConversation | null, startsNew: boolean, key: string): PendingConversation | null {
  return startsNew && pending?.key === key ? pending : null;
}

function notifyStarted(onStarted: ((conversationId: string, prompt: string, images: SentImages['shown']) => void) | undefined, conversationId: string, prompt: string, images: SentImages['shown']): void {
  if (onStarted !== undefined) onStarted(conversationId, prompt, images);
}

/** `/compact` only when asked for: the start request is otherwise as before. */
/** `plan_mode`, and `bypass_approvals` only when it is on. */
function modeFields(mode: TurnMode): Pick<TurnStartRequest, 'plan_mode' | 'bypass_approvals'> {
  return mode === 'bypass' ? { plan_mode: false, bypass_approvals: true } : { plan_mode: mode === 'plan' };
}

function compactFlag(compact: boolean | undefined): { compact?: true } {
  return compact === true ? { compact: true } : {};
}

/** A model only when one was chosen: without it the host runs the thread's kept model. */
function modelField(model: TurnModelChoice | undefined): { model?: TurnModelChoice } {
  return model === undefined ? {} : { model };
}

/** The start request's `images`, only when the message carries some. */
function imagesField(images: SentImages | undefined): { images?: TurnImagePick[] } {
  return images === undefined || images.picks.length === 0 ? {} : { images: images.picks };
}

function shownOf(images: SentImages | undefined): SentImages['shown'] {
  return images === undefined ? [] : images.shown;
}

/** The start request's `command`, only when the message runs one. */
function commandField(command: TurnCommand | undefined): { command?: TurnCommand } {
  return command === undefined ? {} : { command };
}

/** `onStarted`: the host started a turn in `conversationId` (new or continued) — the page makes it the open thread. */
export function useSendPrompt(
  workspace: Workspace,
  projectId: number,
  selection: AgentSelection,
  turn: WorkspaceTurn,
  onStarted?: (conversationId: string, prompt: string, images: SentImages['shown']) => void,
): SendPrompt {
  const ensureConversation = useEnsureConversation();
  const [sendError, setSendError] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  // Synchronous: a second click lands before React re-renders with `sending`.
  const inFlight = useRef(false);
  // A refused start keeps the person's choice ("new conversation"), but the
  // retry runs in the conversation the first attempt created, not another one.
  const pending = useRef<PendingConversation | null>(null);
  const agent = selection.agents.find((a) => a.id === selection.agentId);
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);

  const send = async (prompt: string, mode: TurnMode, mentions: string[], skills: TurnStartRequest['skills'] = [], compact?: boolean, model?: TurnModelChoice, command?: TurnCommand, images?: SentImages): Promise<boolean> => {
    if (agent === undefined || version === undefined || prompt.trim() === '' || turn.busy || inFlight.current) return false;
    inFlight.current = true;
    setSending(true);
    setSendError(null);
    const key = `${String(projectId)}:${agent.id}:${String(version.id)}`;
    const startsNew = selection.conversationId === '';
    const reuse = reusable(pending.current, startsNew, key);
    try {
      const conversationId = await ensureConversation({
        projectId,
        conversationId: reuse === null ? selection.conversationId : reuse.id,
        completeSetup: reuse === null ? false : !reuse.ready,
        onCreated: (id) => {
          if (startsNew) pending.current = { key, id, ready: false };
        },
        prompt,
        applicationId: Number(agent.id),
        applicationName: agent.name,
        versionId: version.id,
        agentType: version.agentType,
        folderName: workspace.name,
      });
      if (startsNew) pending.current = { key, id: conversationId, ready: true };
      const started = await turn.start({
        workspace_id: workspace.id,
        project_id: projectId,
        conversation_id: conversationId,
        application_id: Number(agent.id),
        version_id: version.id,
        prompt,
        ...modeFields(mode),
        mentions,
        skills,
        ...compactFlag(compact),
        ...modelField(model),
        ...commandField(command),
        ...imagesField(images),
      });
      // A refused start (its reason is the turn's startError) keeps the
      // prompt and the person's conversation choice as they were.
      if (!started) return false;
      pending.current = null;
      selection.selectConversation(conversationId);
      notifyStarted(onStarted, conversationId, prompt, shownOf(images));
      return true;
    } catch (error) {
      setSendError(describeWorkspaceError(error));
      return false;
    } finally {
      inFlight.current = false;
      setSending(false);
    }
  };

  const prewarm = (): void => {
    if (agent === undefined || version === undefined) return;
    turn.prewarm({
      project_id: projectId,
      application_id: Number(agent.id),
      version_id: version.id,
      ...(selection.conversationId === '' ? {} : { conversation_id: selection.conversationId }),
    });
  };

  return { canSend: agent !== undefined && version !== undefined && !turn.busy && !sending, sendError, send, prewarm };
}
