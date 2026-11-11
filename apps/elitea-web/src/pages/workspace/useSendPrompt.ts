/**
 * "Send": make sure the conversation exists, then start the turn on the host.
 *
 * In a thread of several agents (ADR-0050 decision 7f) the message's "@Name"
 * mentions are its `addressed_to` (client-core `addressedTo`). Main answers
 * the first addressed agent first, so the turn starts on it (a cloud agent's
 * with `cloud_agent`); the host starts the others in order itself. A
 * composer pick is not an address (decision 7c, #1917): a message that
 * names no agent goes to the thread's lead when it has one, so the turn
 * starts on the lead; a start Main routes elsewhere anyway is refused by the
 * host before any agent runs, and is sent once more to the routed agent.
 */
import { useRef, useState } from 'react';

import { addressedTo, FLOOR_POLICY, floorOf } from '@elitea/client-core/conversation';

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

/** The start fields that run the turn as `agent`. */
function startedOn(agent: ThreadAgent): Pick<TurnStartRequest, 'cloud_agent' | 'application_id' | 'version_id'> {
  return { application_id: agent.applicationId, version_id: agent.versionId, ...(agent.cloud ? { cloud_agent: true } : {}) };
}

/**
 * Who a message is for, in a thread of several agents, as Main routes it (`turntaking.Route`): the agents it names are
 * its `addressed_to`, and the first of them answers first. A composer pick is not an address (ADR-0050 decision 7c,
 * #1917): a message that names no agent carries no `addressed_to`, and in a thread with a lead (`meta.floor`) the lead
 * answers it, so the turn starts on the lead. One agent, or no address and no lead: the composer's agent, as before.
 */
export function floorFields(prompt: string, agents: readonly ThreadAgent[], meta?: unknown): Pick<TurnStartRequest, 'addressed_to' | 'cloud_agent' | 'application_id' | 'version_id'> | Record<string, never> {
  if (agents.length < 2) return {};
  const addressed = addressedTo(prompt, agents);
  const first = agents.find((agent) => agent.participantId === addressed[0]);
  if (first !== undefined) return { addressed_to: addressed, ...startedOn(first) };
  const floor = floorOf(meta);
  const lead = floor.policy === FLOOR_POLICY.Lead ? agents.find((agent) => agent.participantId === floor.leadParticipantId) : undefined;
  return lead === undefined ? {} : startedOn(lead);
}

/** The thread as Main routes a message in it: its agent participants and its `meta` (the lead, `meta.floor`). */
interface ThreadFloor {
  agents: ThreadAgent[];
  meta: unknown;
}

const NO_FLOOR: ThreadFloor = { agents: [], meta: undefined };

/**
 * A message routed to several agents is not a "Run to completion" chain (the host follows the floor instead, one agent
 * after another). A cloud agent's turn takes none of the composer's sticky modes: no grant, model choice, plan mode or
 * bypass (the host would refuse them, `cloud_agent_text_only`), so they are dropped, not sent to be refused. What the
 * person attached to this message (files, images, a command, skills) is sent as it is: the host refuses it with its
 * reason rather than the message going without it.
 */
export function routedRequest(request: TurnStartRequest): TurnStartRequest {
  const routed = (request.addressed_to?.length ?? 0) > 1;
  const cloud = request.cloud_agent === true;
  if (!routed && !cloud) return request;
  const { unattended: _chain, model, bypass_approvals: bypass, ...rest } = request;
  if (cloud) return { ...rest, plan_mode: false };
  return { ...rest, ...(model === undefined ? {} : { model }), ...(bypass === undefined ? {} : { bypass_approvals: bypass }) };
}

/** The thread's other agents, as the conversation adds them (the composer's agent is never its own companion). */
function companionsOf(selection: AgentSelection, agentId: string): CompanionAgent[] {
  return (selection.companions ?? [])
    .filter((c) => c.agentId !== agentId)
    .map((c) => ({ applicationId: Number(c.agentId), applicationName: c.name, versionId: c.versionId, agentType: c.agentType, cloud: c.cloud }));
}

/** The thread's agents and meta, read from the conversation; `NO_FLOOR` when it cannot be read. */
async function readFloor(projectId: number, conversationId: string): Promise<ThreadFloor> {
  try {
    const detail = await conversationApi.details({ projectId, id: conversationId });
    return { agents: threadAgentsOf(detail.participants), meta: detail.meta };
  } catch {
    // The thread could not be read: the composer's agent answers, as in a thread of one (and the host refuses a start
    // Main routes elsewhere, `floor_routed_elsewhere`).
    return NO_FLOOR;
  }
}

/**
 * The thread a message is routed in. A conversation created by this send has no lead and only the agents it added. An
 * existing one is read (its agents and its lead may have changed since the composer opened it): a message that names
 * no agent there goes to the lead, when it has one.
 */
async function threadFloor(projectId: number, conversationId: string, existed: boolean, prompt: string, added: ThreadAgent[]): Promise<ThreadFloor> {
  if (!existed) return { agents: added, meta: undefined };
  const read = await readFloor(projectId, conversationId);
  if (read.agents.length > 0) return read;
  // Unreadable: the agents this send added still route an address.
  return { agents: /[@#]/.test(prompt) || added.length > 0 ? added : [], meta: undefined };
}

/** The host's refusal of a start Main routed to another agent than the one it started on (`check_routed`). */
const ROUTED_ELSEWHERE = 'floor_routed_elsewhere';

/**
 * A start Main routed elsewhere (the thread's lead changed since it was read): the same message once more, on the agent
 * Main routes it to as the thread now reads. `undefined` when that is the agent the refused start was on.
 */
async function rerouted(request: TurnStartRequest, projectId: number, conversationId: string, base: TurnStartRequest): Promise<TurnStartRequest | undefined> {
  const floor = await readFloor(projectId, conversationId);
  const again = routedRequest({ ...base, ...floorFields(base.prompt, floor.agents, floor.meta) });
  return again.application_id === request.application_id && again.version_id === request.version_id ? undefined : again;
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
      const floor = await threadFloor(projectId, conversationId, selection.conversationId !== '', prompt, ensured.agents);
      if (startsNew) pending.current = { key, id: conversationId, ready: true };
      const base: TurnStartRequest = {
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
      };
      const request = routedRequest({ ...base, ...floorFields(prompt, floor.agents, floor.meta) });
      // Main routed the message to another agent than the one it started on (the lead changed since the thread was
      // read): once more, on the agent the thread now routes it to.
      const started = await turn.start(request, (refusal) =>
        refusal.code === ROUTED_ELSEWHERE ? rerouted(request, projectId, conversationId, base) : withoutRefusedGrant(request, refusal),
      );
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
