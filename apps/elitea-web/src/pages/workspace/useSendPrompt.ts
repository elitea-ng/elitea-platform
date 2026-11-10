/**
 * "Send": make sure the conversation exists, then start the turn on the host.
 *
 * In a thread of several agents (ADR-0050 decision 7f) the message's "@Name"
 * mentions are its `addressed_to` (client-core `addressedTo`). Main answers
 * the first addressed agent first, so the turn starts on it (a cloud agent's
 * with `cloud_agent`); the host starts the others in order itself.
 */
import { useRef, useState } from 'react';

import { addressedTo } from '@elitea/client-core/conversation';

import { conversationApi } from '@/entities/conversation';
import { isDesktopGrantsOff, markGrantsTurnedOff } from '@/features/unattended';
import { describeWorkspaceError, type WorkspaceTurn } from '@/features/workspace';
import type { TurnModelChoice } from '@/shared/desktop/turnModel';
import type { AttachedImage, TurnCommand, TurnImagePick, TurnModeChoice, TurnStartRequest, Workspace } from '@/shared/desktop/workspaceIpc';

import type { AgentSelection } from './useAgentSelection';
import { modeFields } from './turnModeFields';
import { threadAgentsOf, useEnsureConversation, type CompanionAgent, type ThreadAgent } from './useEnsureConversation';

/** The images a message carries: the tokens the host holds them by, and what the transcript shows of them. */
export interface SentImages {
  picks: TurnImagePick[];
  shown: readonly Pick<AttachedImage, 'name' | 'thumbnail'>[];
}

export interface SendPrompt {
  canSend: boolean;
  sendError: string | null;
  /** `mentions`: the workspace paths the prompt references with "@"; `skills`: the skills it invokes with "/"; `compact`: `/compact` was asked for; `model`: the model chip's choice; `command`: the custom command it runs. */
  send: (prompt: string, mode: TurnModeChoice, mentions: string[], skills?: TurnStartRequest['skills'], compact?: boolean, model?: TurnModelChoice, command?: TurnCommand, images?: SentImages) => Promise<boolean>;
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

/**
 * ADR-0045: "Run to completion" is on by default, but an administrator can
 * turn grants off. Then the control hides for this page load. A grant with
 * only the continue parts goes once more without the grant (an older Main
 * refuses it); a grant with hook asks or remote confirmations shows the
 * refusal, because the person asked for those answers.
 */
function withoutRefusedGrant(request: TurnStartRequest, refusal: { code: string; message: string }): TurnStartRequest | undefined {
  const grant = request.unattended;
  if (grant === undefined || !isDesktopGrantsOff(refusal.code)) return undefined;
  markGrantsTurnedOff();
  if (grant.hook_asks || grant.remote_confirmations) return undefined;
  const { unattended: _refused, ...rest } = request;
  return rest;
}

function shownOf(images: SentImages | undefined): SentImages['shown'] {
  return images === undefined ? [] : images.shown;
}

/**
 * Who a message is for, in a thread of several agents: `addressed_to`, and the agent that answers first (the first one
 * addressed), on its own application and version. One agent, or none addressed: the composer's agent, as before.
 */
export function floorFields(prompt: string, agents: readonly ThreadAgent[]): Pick<TurnStartRequest, 'addressed_to' | 'cloud_agent' | 'application_id' | 'version_id'> | Record<string, never> {
  if (agents.length < 2) return {};
  const addressed = addressedTo(prompt, agents);
  const first = agents.find((agent) => agent.participantId === addressed[0]);
  if (first === undefined) return {};
  return { addressed_to: addressed, application_id: first.applicationId, version_id: first.versionId, ...(first.cloud ? { cloud_agent: true } : {}) };
}

/**
 * A message routed to several agents is not a "Run to completion" chain (the host follows the floor instead, one agent
 * after another), and a cloud agent's turn takes neither the grant nor a model choice (the host refuses them,
 * `cloud_agent_text_only`): the composer's defaults are dropped, not sent to be refused.
 */
export function routedRequest(request: TurnStartRequest): TurnStartRequest {
  const routed = (request.addressed_to?.length ?? 0) > 1;
  const cloud = request.cloud_agent === true;
  if (!routed && !cloud) return request;
  const { unattended: _chain, model, ...rest } = request;
  return cloud || model === undefined ? rest : { ...rest, model };
}

/** The thread's other agents, as the conversation adds them (the composer's agent is never its own companion). */
function companionsOf(selection: AgentSelection, agentId: string): CompanionAgent[] {
  return (selection.companions ?? [])
    .filter((c) => c.agentId !== agentId)
    .map((c) => ({ applicationId: Number(c.agentId), applicationName: c.name, versionId: c.versionId, agentType: c.agentType, cloud: c.cloud }));
}

/**
 * The thread's agents a message may address: those the send just added, else, for a message that mentions someone in
 * a thread reopened later, the conversation's own agent participants.
 */
async function addressableAgents(projectId: number, conversationId: string, prompt: string, added: ThreadAgent[]): Promise<ThreadAgent[]> {
  if (added.length > 0 || !/[@#]/.test(prompt)) return added;
  try {
    return threadAgentsOf((await conversationApi.details({ projectId, id: conversationId })).participants);
  } catch {
    // The participants could not be read: the composer's agent answers, as in a thread of one.
    return [];
  }
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

  const send = async (prompt: string, mode: TurnModeChoice, mentions: string[], skills: TurnStartRequest['skills'] = [], compact?: boolean, model?: TurnModelChoice, command?: TurnCommand, images?: SentImages): Promise<boolean> => {
    if (agent === undefined || version === undefined || prompt.trim() === '' || turn.busy || inFlight.current) return false;
    inFlight.current = true;
    setSending(true);
    setSendError(null);
    const key = `${String(projectId)}:${agent.id}:${String(version.id)}`;
    const startsNew = selection.conversationId === '';
    const reuse = reusable(pending.current, startsNew, key);
    try {
      const ensured = await ensureConversation({
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
        companions: companionsOf(selection, agent.id),
      });
      const conversationId = ensured.id;
      const agents = await addressableAgents(projectId, conversationId, prompt, ensured.agents);
      if (startsNew) pending.current = { key, id: conversationId, ready: true };
      const request: TurnStartRequest = routedRequest({
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
        ...floorFields(prompt, agents),
      });
      const started = await turn.start(request, (refusal) => withoutRefusedGrant(request, refusal));
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
