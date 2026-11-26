/**
 * An unattended chain's events (ADR-0045, `unattendedIpc.ts`), folded into the
 * turn's view: `turn_continued` (the host started the next turn by itself),
 * `chain_ended` (the chain's last turn), `run_brief` (the brief, on the chain's
 * first turn), `goal_checked` (the goal check of the turn that would end the
 * chain) and the unattended fields of `status` `running`. The reducer makes
 * `next_turn_id` the active turn (`continuedTurn`).
 *
 * A message addressed to several agents (ADR-0050 decision 7f) goes on the same way: once an agent's commit lands, the
 * host starts the next agent's turn (`turn_continued` kind `floor`), or says `floor_stopped` when it does not answer.
 * A hand-off goes on the same way (kind `hand`, ADR-0061 decision 3d); its side rows are `teamTalkEvents.ts`.
 */
import { goalCheckOfChainEnd, readGoalCheck, readRunBrief, type GoalCheckView, type RunBriefView } from '@/entities/run-brief';
import type { AgentExecutor, ChainEndedPayload, ContinuationKind, ExchangeQueueItem, TurnFloorStatus, UnattendedGrant } from '@/shared/desktop/unattendedIpc';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import { applyTeamTalkEvent, isTeamTalkEvent, type SideTalkItem } from './teamTalkEvents';

type EventOf<K extends AgentEvent['kind']> = Extract<AgentEvent, { kind: K }>;

export type ChainEvent = EventOf<'turn_continued'> | EventOf<'chain_ended'> | EventOf<'run_brief'> | EventOf<'goal_checked'> | EventOf<'resume_offered'> | EventOf<'floor_stopped'> | EventOf<'agent_state'> | EventOf<'exchange'> | TalkEvent;

/** The agent that answers a turn of a message addressed to several (`status` `running`'s `floor`). */
interface TurnAgentView {
  name: string;
  participantId: number;
  executor: AgentExecutor;
  /** Its place in the message's order, from 0; a turn at 1 or later answers the person's message above it. */
  position: number;
  /** How many agents answer the message. */
  targets: number;
  /** The agents still to answer after this one, in the host's order, skips removed (ADR-0061 decision 2c). */
  exchange?: number[];
  /** The agents still to answer with the hand-off that queued each, as the host last sent them (each Skip names its own). */
  queue?: { participantId: number; handId?: string }[];
}

/** The part of a turn's view that the chain sets. */
export interface ChainView {
  /** The host started this turn by itself: the n-th continuation of the chain (from 1). */
  continuation?: number;
  /** The host started this turn after an app restart (link kind `resume`). */
  resumed?: true;
  /** The grant the turn runs under (the first `running` of an unattended turn). */
  unattended?: Omit<UnattendedGrant, 'chain'> & { grant_id: string };
  /** The host continued this turn in `nextTurnId` by itself: the person does not click Continue. */
  continuedTo?: { nextTurnId: string; index: number; kind?: ContinuationKind; agentName?: string; executor?: AgentExecutor };
  /** The agent that answers this turn, when the message went to several. */
  agent?: TurnAgentView;
  /** The message's next agent does not answer (`floor_stopped`): a newer message took the floor, or its start failed. */
  floorStopped?: { code: string; message: string; guard?: boolean };
  /** Agents talking to each other in this turn: its hand-off and its consults (ADR-0061 decisions 3d, 3e). */
  sideTalk?: SideTalkItem[];
  /** This turn is the last of its chain. */
  chainEnded?: ChainEndedPayload;
  /** The chain's run brief (its first turn only). */
  runBrief?: RunBriefView;
  /** The goal check of this turn: from `goal_checked`, else the short form of `chain_ended.goal`. */
  goalCheck?: GoalCheckView;
  /** The answering agent's last avatar state (`agent_state`, ADR-0061 decision 4b): the host's frame, as sent. */
  agentState?: EventOf<'agent_state'>['payload'];
}

export function isChainEvent(event: AgentEvent): event is ChainEvent {
  const kinds: readonly AgentEvent['kind'][] = ['turn_continued', 'chain_ended', 'run_brief', 'goal_checked', 'resume_offered', 'floor_stopped', 'agent_state', 'exchange'];
  return kinds.includes(event.kind) || isTeamTalkEvent(event);
}

function continuedTo(payload: EventOf<'turn_continued'>['payload']): NonNullable<ChainView['continuedTo']> {
  const { next_turn_id: nextTurnId, continuation_index: index, kind, agent_name: agentName, executor } = payload;
  const next: NonNullable<ChainView['continuedTo']> = { nextTurnId, index };
  if (kind !== undefined) next.kind = kind;
  if (agentName !== undefined) next.agentName = agentName;
  if (executor !== undefined) next.executor = executor;
  return next;
}

export function applyChainEvent<V extends ChainView>(view: V, event: ChainEvent): V {
  if (isBesideEvent(event)) return applyBesideEvent(view, event);
  switch (event.kind) {
    case 'chain_ended': {
      // The full check of `goal_checked` wins over the short form the chain end repeats.
      const goal = view.goalCheck ?? goalCheckOfChainEnd(event.payload.goal);
      return { ...view, chainEnded: event.payload, ...(goal === undefined ? {} : { goalCheck: goal }) };
    }
    case 'run_brief': {
      const brief = readRunBrief(event.payload);
      return brief === undefined ? view : { ...view, runBrief: brief };
    }
    case 'goal_checked': {
      const check = readGoalCheck(event.payload);
      return check === undefined ? view : { ...view, goalCheck: check };
    }
    case 'turn_continued':
      return { ...view, continuedTo: continuedTo(event.payload) };
    case 'floor_stopped':
      return { ...view, floorStopped: event.payload };
    // On a recorded turn: the page offers Continue (`resumeEvents.ts`); the view does not change.
    case 'resume_offered':
      return view;
  }
}

type TalkEvent = Parameters<typeof applyTeamTalkEvent>[1];

/** The live-only events and the agents' side talk: not the chain's own. */
function isBesideEvent(event: ChainEvent): event is LiveEvent | TalkEvent {
  return isLiveEvent(event) || isTeamTalkEvent(event);
}

function applyBesideEvent<V extends ChainView>(view: V, event: LiveEvent | TalkEvent): V {
  return isTeamTalkEvent(event) ? applyTeamTalkEvent(view, event) : applyLiveEvent(view, event);
}

type LiveEvent = EventOf<'agent_state'> | EventOf<'exchange'>;

function isLiveEvent(event: ChainEvent): event is LiveEvent {
  return event.kind === 'agent_state' || event.kind === 'exchange';
}

/** The host's live-only events (ADR-0061): the agent's state, and the exchange's queue after a skip. Never recorded. */
function applyLiveEvent<V extends ChainView>(view: V, event: LiveEvent): V {
  if (event.kind === 'agent_state') return { ...view, agentState: event.payload };
  if (view.agent === undefined) return view;
  // The host's queue replaces the last one whole: nothing carries over from an earlier event.
  const { queue: _before, ...agent } = view.agent;
  return { ...view, agent: { ...agent, exchange: event.payload.remaining, ...queueOf(event.payload.queue) } };
}

/** The host's queue on the view (absent when the host sent none). */
function queueOf(wire: readonly ExchangeQueueItem[] | null | undefined): Pick<TurnAgentView, 'queue'> {
  if (wire === undefined || wire === null) return {};
  return { queue: wire.map((item) => ({ participantId: item.participant_id, ...(item.hand_id === undefined ? {} : { handId: item.hand_id }) })) };
}

/**
 * The turn is done, but the host starts the chain's next turn by itself and has not said so yet: the composer stays
 * busy until `turn_continued` or `chain_ended` (the host sends `chain_ended` itself when the next turn does not start
 * within 60 s).
 */
export function waitsForNextTurn(view: ChainView & { done?: { continuing?: true } }): boolean {
  return view.done?.continuing === true && view.continuedTo === undefined && view.chainEnded === undefined && view.floorStopped === undefined;
}

function agentOf(floor: TurnFloorStatus): TurnAgentView {
  return { name: floor.agent_name, participantId: floor.participant_id, executor: floor.executor, position: floor.position, targets: floor.targets, ...(floor.exchange === undefined ? {} : { exchange: floor.exchange }), ...queueOf(floor.queue) };
}

/** The unattended fields of a `status` event, when it has them. */
export function withChainStatus<V extends ChainView>(view: V, payload: EventOf<'status'>['payload']): V {
  let next = view;
  if (payload.continued_automatically === true) next = { ...next, continuation: payload.continuation_index ?? 1 };
  if (payload.continued_automatically === true && payload.continuation_kind === 'resume') next = { ...next, resumed: true };
  if (payload.unattended !== undefined) next = { ...next, unattended: payload.unattended };
  if (payload.floor !== undefined) next = { ...next, agent: agentOf(payload.floor) };
  return next;
}

/**
 * The turn that `event` (a `turn_continued` on a turn this hook began, `begun`) makes the active one, as a `begin`
 * does; null for any other event, and for a late delivery (the next turn is begun already).
 */
export function continuedTurn(begun: readonly string[], event: AgentEvent): string | null {
  if (event.kind !== 'turn_continued' || !begun.includes(event.turn_id)) return null;
  return begun.includes(event.payload.next_turn_id) ? null : event.payload.next_turn_id;
}
