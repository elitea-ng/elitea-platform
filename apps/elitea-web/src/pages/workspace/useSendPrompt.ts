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
 *
 * The thread's agents and lead are read once and kept a short while per
 * thread (`threadFloorQuery`, #1928), shared with the composer's "who
 * answers" notice; a refusal that says they changed reads them again.
 */
import { useRef, useState } from 'react';

import { useQueryClient, type QueryClient } from '@tanstack/react-query';

import { addressedTo, firstAnswerer, floorOf } from '@elitea/client-core/conversation';

import { conversationApi } from '@/entities/conversation';
import { defaultRunVersionId } from '@/entities/version';
import { getApplication } from '@/shared/api/generated/applications/applications';
import { unwrapBody } from '@/shared/api/unwrap';
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

/** The start fields that run the turn as `agent`: its participant (#1927), application and version, and where it runs. */
function startedOn(agent: ThreadAgent): Pick<TurnStartRequest, 'cloud_agent' | 'application_id' | 'version_id' | 'participant_id'> {
  return { application_id: agent.applicationId, version_id: agent.versionId, participant_id: agent.participantId, ...(agent.cloud ? { cloud_agent: true } : {}) };
}

/** The fields `floorFields` sets: the address, and the agent the turn starts on. */
export type FloorFields = Pick<TurnStartRequest, 'addressed_to' | 'cloud_agent' | 'application_id' | 'version_id' | 'participant_id'>;

/**
 * Who a message is for, in a thread of several agents, as Main routes it (client-core `firstAnswerer`, the one copy of
 * `turntaking.Route` the clients share, #1932): the agents it names are its `addressed_to`, and the first of them answers
 * first. A composer pick is not an address (ADR-0050 decision 7c, #1917): a message that names no agent carries no
 * `addressed_to`, and in a thread with a lead (`meta.floor`) the lead answers it, so the turn starts on the lead. One
 * agent, or no address and no lead: the composer's agent, as before. An unpinned agent's `version_id` is 0 here:
 * `withVersion` reads its default version before the start (#1926).
 */
export function floorFields(prompt: string, agents: readonly ThreadAgent[], meta?: unknown): FloorFields | Record<string, never> {
  if (agents.length < 2) return {};
  const addressed = addressedTo(prompt, agents);
  const first = agents.find((agent) => agent.participantId === firstAnswerer(addressed, floorOf(meta), agents));
  if (first === undefined) return {};
  return addressed.length > 0 ? { addressed_to: addressed, ...startedOn(first) } : startedOn(first);
}

/**
 * An agent's default version, from its detail (`GET application`): `meta.default_version_id`, else the one named
 * `base`, else its first (`defaultRunVersionId`, the one rule; the host reads a later unpinned agent's the same way).
 * 0 when it has none.
 */
export function defaultVersionOf(detail: unknown): number {
  const record = (detail ?? {}) as { versions?: unknown; meta?: { default_version_id?: unknown } | null };
  const versions = Array.isArray(record.versions) ? (record.versions as { id?: unknown; name?: unknown }[]) : [];
  return defaultRunVersionId(versions, record.meta?.default_version_id) ?? 0;
}

/** What a start sends of the floor: the fields that run the routed agent, or only the address (or nothing). */
type StartFloor = FloorFields | Pick<TurnStartRequest, 'addressed_to'>;

/**
 * The floor fields with a version to run: an unpinned agent (version 0) runs its agent's default version, read here
 * (#1926). Unreadable or versionless: the message keeps its address, so Main still routes it to the agents it names,
 * and starts on the composer's agent; the host refuses that start (`floor_routed_elsewhere`) and says why.
 */
export async function withVersion(projectId: number, fields: FloorFields | Record<string, never>): Promise<StartFloor> {
  if (fields.version_id !== 0 || fields.application_id === undefined) return fields;
  const floor = fields as FloorFields;
  const addressOnly: StartFloor = floor.addressed_to === undefined ? {} : { addressed_to: floor.addressed_to };
  try {
    const version = defaultVersionOf(unwrapBody(await getApplication(String(projectId), floor.application_id)));
    return version > 0 ? { ...floor, version_id: version } : addressOnly;
  } catch {
    return addressOnly;
  }
}

/** The thread as Main routes a message in it: its agent participants and its `meta` (the lead, `meta.floor`). */
export interface ThreadFloor {
  agents: ThreadAgent[];
  meta: unknown;
}

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

/** How long a thread's agents and lead are kept before a send reads them again (#1928). */
export const THREAD_FLOOR_STALE_MS = 30_000;

/** The query of a thread's floor: one per thread, shared by every send and the composer's notice. */
export function threadFloorQuery(projectId: number, conversationId: string): { queryKey: readonly unknown[]; queryFn: () => Promise<ThreadFloor>; staleTime: number } {
  return {
    queryKey: ['workspace', 'threadFloor', projectId, conversationId] as const,
    queryFn: async () => {
      const detail = await conversationApi.details({ projectId, id: conversationId });
      return { agents: threadAgentsOf(detail.participants), meta: detail.meta };
    },
    staleTime: THREAD_FLOOR_STALE_MS,
  };
}

/**
 * The thread's agents and meta, as kept (`fresh`: read again now, the kept copy is out of date); `undefined` when the
 * thread cannot be read.
 */
async function readFloor(queryClient: QueryClient, projectId: number, conversationId: string, fresh: boolean): Promise<ThreadFloor | undefined> {
  const query = threadFloorQuery(projectId, conversationId);
  try {
    return await queryClient.query(fresh ? { ...query, staleTime: 0 } : query);
  } catch {
    return undefined;
  }
}

/**
 * The thread a message is routed in. A conversation created by this send has no lead and only the agents it added. An
 * existing one is read (kept a short while, #1928; read again when this send added agents to it): a message that
 * names no agent there goes to the lead, when it has one. Unreadable, the agents this send added still route an
 * address, and the composer's agent answers otherwise (the host refuses a start Main routes elsewhere,
 * `floor_routed_elsewhere`).
 */
async function threadFloor(queryClient: QueryClient, projectId: number, conversationId: string, existed: boolean, added: ThreadAgent[]): Promise<ThreadFloor> {
  if (!existed) return { agents: added, meta: undefined };
  const read = await readFloor(queryClient, projectId, conversationId, added.length > 0);
  if (read !== undefined && read.agents.length > 0) return read;
  return { agents: added, meta: undefined };
}

/** The host's refusal of a start Main routed to another agent than the one it started on (`check_routed`). */
const ROUTED_ELSEWHERE = 'floor_routed_elsewhere';

/**
 * A start Main routed elsewhere (the thread's lead changed since it was read): the same message once more, on the agent
 * Main routes it to as the thread now reads (read again, the kept copy is out of date). `undefined` (the refusal shows)
 * when the thread cannot be read (the retry would only be refused again, #1931) or routes to the agent the refused
 * start was on.
 */
export async function rerouted(queryClient: QueryClient, refused: TurnStartRequest, projectId: number, conversationId: string, base: TurnStartRequest): Promise<TurnStartRequest | undefined> {
  const floor = await readFloor(queryClient, projectId, conversationId, true);
  if (floor === undefined) return undefined;
  const again = routedRequest({ ...base, ...(await withVersion(projectId, floorFields(base.prompt, floor.agents, floor.meta))) });
  // A retry that names no participant answers on whichever one the version finds: no other agent than the refused one.
  const participant = again.participant_id === undefined || again.participant_id === refused.participant_id;
  const same = again.application_id === refused.application_id && again.version_id === refused.version_id && participant;
  return same ? undefined : again;
}

/**
 * What a refused start is sent again as (`turn.start`'s retry, which may run more than once: each retry's refusal
 * comes back here, #1931): on the agent Main routes it to, or without a grant the platform turned off. A grant
 * dropped once stays dropped for a later reroute.
 */
function refusalChain(queryClient: QueryClient, projectId: number, conversationId: string, base: TurnStartRequest) {
  let current = base;
  return async (refusal: { code: string; message: string }, refused: TurnStartRequest): Promise<TurnStartRequest | undefined> => {
    if (refusal.code === ROUTED_ELSEWHERE) return rerouted(queryClient, refused, projectId, conversationId, current);
    const without = withoutRefusedGrant(refused, refusal);
    if (without !== undefined) {
      const { unattended: _refused, ...rest } = current;
      current = rest;
    }
    return without;
  };
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
  const queryClient = useQueryClient();
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
      const floor = await threadFloor(queryClient, projectId, conversationId, selection.conversationId !== '', ensured.agents);
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
      const request = routedRequest({ ...base, ...(await withVersion(projectId, floorFields(prompt, floor.agents, floor.meta))) });
      // Main routed the message to another agent than the one it started on (the lead changed since the thread was
      // read): once more, on the agent the thread now routes it to; a grant the platform turned off goes once more
      // without it. Each retry's refusal runs through the same chain (#1931).
      const started = await turn.start(request, refusalChain(queryClient, projectId, conversationId, base));
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
