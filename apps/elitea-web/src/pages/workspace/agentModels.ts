/**
 * The model each agent of a desktop thread runs on (#3145, IPC.md "Model per agent"). Every agent runs on its own
 * configured model; the person may choose another one for an agent in this thread: from that agent's row in the
 * Participants tab ("Model in this thread…"), its row under Thread settings › Models, or `/model` (the lead, else the
 * composer's agent). There is no thread-wide model chip any more: with a team, one choice for "the thread" did not say
 * which member it was for.
 *
 * - What the host keeps: each agent's session keeps the choice it last ran on (with two or more agents each agent has a
 *   session of its own; with one, the thread's). It is read back from the turns' `status` `running` `model` (`chosen`
 *   true: the person's choice), each turn's agent from its `floor`. A turn without `floor` ran the thread's one agent, or
 *   predates the team: its choice is the lead's (else the composer's agent's). That is how a thread-wide choice made
 *   before this change keeps applying: to the lead, or to the single agent.
 * - What the person chose here that no turn ran on yet (`pending`), by agent (application id), for this visit.
 *
 * A send carries the choices made here (`agent_models`): the turn's agent runs on its own, and so does a later agent of
 * the message that the host starts itself (a routed agent, a hand-off). A kept choice is not sent again: the agent's
 * session applies it, and the host re-checks it there (pinned since, turned off, no longer listed). Choosing the agent's
 * own model puts it back on it (the host then keeps no choice). A cloud agent runs on its own version's model: it has
 * no choice here. The choices made before the first message go with it to a new worktree's thread (`handed`, #3144).
 */
import { useMemo, useState, useSyncExternalStore } from 'react';

import { floorOf } from '@elitea/client-core/conversation';
import { replayView, type WorkspaceTurn } from '@/features/workspace';
import { toReasoningEffort, type TurnModel, type TurnModelChoice } from '@/shared/desktop/turnModel';
import type { TurnStartRequest } from '@/shared/desktop/workspaceIpc';

import { useThreadFloor } from './useComposerMentions';
import type { ThreadAgent } from './useEnsureConversation';
import type { ThreadFloor } from './useSendPrompt';
import type { ThreadHistory } from './useThreadHistory';

/** One entry of `agent_turn_start.agent_models`. */
export type AgentModelEntry = NonNullable<TurnStartRequest['agent_models']>[number];

/** A choice made here, and the agent's own model when it was made (choosing that one shows no badge). */
interface PendingChoice {
  readonly choice: TurnModelChoice;
  readonly own: string | null;
}

/** The choices made in a folder's new thread, handed with its first message to the worktree's thread (#3144). */
export type HandedAgentModels = ReadonlyArray<readonly [number, PendingChoice]>;

/** One turn as the kept choices read it: its agent (from `floor`) and the model it ran on. */
export interface TurnModelRecord {
  readonly participantId?: number | undefined;
  readonly model?: TurnModel | undefined;
}

/**
 * The choices the host keeps, by agent (application id), from the thread's turns oldest first. A turn without an agent
 * ran the thread's one agent, else the lead (`leadApplicationId`), else the composer's agent.
 */
export function keptModels(turns: readonly TurnModelRecord[], agents: readonly ThreadAgent[], leadApplicationId: number): Map<number, TurnModelChoice> {
  const kept = new Map<number, TurnModelChoice>();
  for (const turn of turns) {
    if (turn.model === undefined) continue;
    const applicationId = turn.participantId === undefined ? leadApplicationId : agents.find((agent) => agent.participantId === turn.participantId)?.applicationId;
    if (applicationId === undefined || applicationId <= 0) continue;
    if (turn.model.chosen) {
      const effort = toReasoningEffort(turn.model.reasoning_effort);
      kept.set(applicationId, effort === undefined ? { name: turn.model.name } : { name: turn.model.name, reasoning_effort: effort });
    } else kept.delete(applicationId);
  }
  return kept;
}

/** The thread's turns, oldest first: the recorded ones not shown live, then this visit's. */
export function turnModelRecords(turn: Pick<WorkspaceTurn, 'turnId' | 'view' | 'earlier'>, recorded: readonly ({ turnId: string } & TurnModelRecord)[]): TurnModelRecord[] {
  const live = new Set(turn.earlier.map((entry) => entry.turnId));
  if (turn.turnId !== null) live.add(turn.turnId);
  const record = (view: WorkspaceTurn['view']): TurnModelRecord => ({ participantId: view.agent?.participantId, model: view.model });
  return [...recorded.filter((entry) => !live.has(entry.turnId)), ...turn.earlier.map((entry) => record(entry.view)), ...(turn.turnId === null ? [] : [record(turn.view)])];
}

function recordedModels(history: ThreadHistory): ({ turnId: string } & TurnModelRecord)[] {
  if (history.kind !== 'local') return [];
  return history.turns.map((stored) => {
    const view = replayView(stored.events);
    return { turnId: stored.turn_id, participantId: view.agent?.participantId, model: view.model };
  });
}

export interface AgentModels {
  /** The agent's choice in this thread (application id); `undefined`: it runs on its own model. */
  overrideOf: (applicationId: number) => TurnModelChoice | undefined;
  /** Chooses `choice` for the agent; `own` is its configured model (choosing it puts the agent back on it). */
  choose: (applicationId: number, choice: TurnModelChoice, own: string | null) => void;
  /** What a send carries (`agent_models`): the choices made here (the host keeps and applies the earlier ones). */
  entries: () => AgentModelEntry[];
  /** The choices made here, to hand to a new worktree's thread with the first message. */
  handed: () => HandedAgentModels;
  /** The agent whose model menu is to open (a row's "Model in this thread…", `/model`); `null`: none. */
  editing: number | null;
  edit: (applicationId: number | null) => void;
  /** The host refused a start `model_unavailable`: the choices made here go, so the next send does not carry them again. */
  dropPending: () => void;
}

interface Store {
  pending: Map<number, PendingChoice>;
  editing: number | null;
  version: number;
  listeners: Set<() => void>;
}


function changed(store: Store): void {
  store.version += 1;
  store.listeners.forEach((listener) => listener());
}

/** The thread's choices made here: one store per session page, shared by the composer and the Participants tab. */
export function useAgentModelStore(seed?: HandedAgentModels): Store {
  const [store] = useState<Store>(() => ({ pending: new Map(seed), editing: null, version: 0, listeners: new Set() }));
  useSyncExternalStore(
    (listener) => {
      store.listeners.add(listener);
      return () => store.listeners.delete(listener);
    },
    () => store.version,
  );
  return store;
}

const isOwn = (pending: PendingChoice): boolean => pending.own !== null && pending.choice.name === pending.own && pending.choice.reasoning_effort === undefined;

/** The store's view with the choices the host keeps (`kept`, from `keptModels`). */
export function agentModelsOf(store: Store, kept: ReadonlyMap<number, TurnModelChoice>): AgentModels {
  return {
    overrideOf: (applicationId) => {
      const pending = store.pending.get(applicationId);
      if (pending === undefined) return kept.get(applicationId);
      return isOwn(pending) ? undefined : pending.choice;
    },
    choose: (applicationId, choice, own) => {
      if (applicationId <= 0) return;
      store.pending.set(applicationId, { choice, own });
      changed(store);
    },
    entries: () =>
      [...store.pending].map(([applicationId, { choice }]) => ({
        application_id: applicationId,
        name: choice.name,
        ...(choice.reasoning_effort === undefined ? {} : { reasoning_effort: choice.reasoning_effort }),
      })),
    handed: () => [...store.pending],
    editing: store.editing,
    edit: (applicationId) => {
      store.editing = applicationId;
      changed(store);
    },
    dropPending: () => {
      if (store.pending.size === 0) return;
      store.pending.clear();
      changed(store);
    },
  };
}

/**
 * The thread's agent models: `agents` are the thread's (none before its first message), `leadApplicationId` the agent a
 * turn without `floor` ran (the lead, else the single or composer's agent).
 */
function useThreadAgentModels(store: Store, turn: Pick<WorkspaceTurn, 'turnId' | 'view' | 'earlier'>, history: ThreadHistory, agents: readonly ThreadAgent[], leadApplicationId: number): AgentModels {
  const recorded = useMemo(() => recordedModels(history), [history]);
  const records = turnModelRecords(turn, recorded);
  const kept = keptModels(records, agents, leadApplicationId);
  // `store.version` changes with every choice: the view is rebuilt on each render, as the store hook re-renders.
  return agentModelsOf(store, kept);
}

const NO_AGENTS: readonly ThreadAgent[] = [];

/**
 * The agent a turn without `floor` ran: the thread's one agent, else its lead, else the composer's agent. A thread-wide
 * model chosen before each agent had its own is read as that agent's.
 */
function threadAgentOf(floor: ThreadFloor | undefined, composerAgentId: string): number {
  const agents = floor?.agents ?? [];
  if (agents.length === 1) return agents[0]?.applicationId ?? 0;
  const lead = floor === undefined ? 0 : floorOf(floor.meta).leadParticipantId;
  return agents.find((agent) => agent.participantId === lead)?.applicationId ?? (Number(composerAgentId) || 0);
}

/** The session page's agent models: the store, the thread's agents and the turns it read. */
export function useSessionAgentModels(projectId: number, conversationId: string, turn: Pick<WorkspaceTurn, 'turnId' | 'view' | 'earlier'>, history: ThreadHistory, composerAgentId: string, handed?: { readonly agentModels?: HandedAgentModels | undefined }): AgentModels {
  const store = useAgentModelStore(handed?.agentModels);
  const floor = useThreadFloor(projectId, conversationId);
  return useThreadAgentModels(store, turn, history, floor?.agents ?? NO_AGENTS, threadAgentOf(floor, composerAgentId));
}
