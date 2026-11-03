/**
 * An unattended chain's events (ADR-0045, `unattendedIpc.ts`), folded into the
 * turn's view: `turn_continued` (the host started the next turn by itself),
 * `chain_ended` (the chain's last turn) and the unattended fields of
 * `status` `running`. The reducer makes `next_turn_id` the active turn
 * (`continuedTurn`).
 */
import type { ChainEndedPayload, UnattendedGrant } from '@/shared/desktop/unattendedIpc';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

type EventOf<K extends AgentEvent['kind']> = Extract<AgentEvent, { kind: K }>;

export type ChainEvent = EventOf<'turn_continued'> | EventOf<'chain_ended'>;

/** The part of a turn's view that the chain sets. */
export interface ChainView {
  /** The host started this turn by itself: the n-th continuation of the chain (from 1). */
  continuation?: number;
  /** The grant the turn runs under (the first `running` of an unattended turn). */
  unattended?: Omit<UnattendedGrant, 'chain'> & { grant_id: string };
  /** The host continued this turn in `nextTurnId` by itself: the person does not click Continue. */
  continuedTo?: { nextTurnId: string; index: number };
  /** This turn is the last of its chain. */
  chainEnded?: ChainEndedPayload;
}

export function isChainEvent(event: AgentEvent): event is ChainEvent {
  return event.kind === 'turn_continued' || event.kind === 'chain_ended';
}

export function applyChainEvent<V extends ChainView>(view: V, event: ChainEvent): V {
  if (event.kind === 'chain_ended') return { ...view, chainEnded: event.payload };
  return { ...view, continuedTo: { nextTurnId: event.payload.next_turn_id, index: event.payload.continuation_index } };
}

/**
 * The turn is done, but the host starts the chain's next turn by itself and has not said so yet: the composer stays
 * busy until `turn_continued` or `chain_ended` (the host sends `chain_ended` itself when the next turn does not start
 * within 60 s).
 */
export function waitsForNextTurn(view: ChainView & { done?: { continuing?: true } }): boolean {
  return view.done?.continuing === true && view.continuedTo === undefined && view.chainEnded === undefined;
}

/** The unattended fields of a `status` event, when it has them. */
export function withChainStatus<V extends ChainView>(view: V, payload: EventOf<'status'>['payload']): V {
  let next = view;
  if (payload.continued_automatically === true) next = { ...next, continuation: payload.continuation_index ?? 1 };
  if (payload.unattended !== undefined) next = { ...next, unattended: payload.unattended };
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
