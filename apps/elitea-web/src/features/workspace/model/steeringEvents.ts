/**
 * The steering queue of a turn (IPC.md "Steering"), folded into the turn's view.
 *
 * - `steer_queued` adds a waiting message, or changes it (a higher `revision` wins).
 * - `steer_injected` takes it out of the queue and adds it to the transcript as a user message, at the point of
 *   the turn where the agent got it. A reloaded thread folds the same events, so it shows the message there too.
 * - `steer_dropped` takes it out of the queue. With reason `turn_ended` the turn ended before a safe point: the
 *   text goes to `endedSteers`, which the composer sends as the next turn.
 */
import type { SteeringEvent } from '@/shared/desktop/steeringIpc';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import type { TranscriptItem } from './transcriptItem';

/** A message that waits for the next safe point. */
export interface PendingSteer {
  steerId: string;
  text: string;
  revision: number;
}

/** The part of a turn's view these events change. */
export interface SteerView {
  items: TranscriptItem[];
  /** The messages that wait for the agent, oldest first; absent when none ever waited. */
  pendingSteers?: PendingSteer[] | undefined;
  /** The messages the turn ended before taking, oldest first: the next turn's message. */
  endedSteers?: { steerId: string; text: string }[] | undefined;
}

type SteerEvent = AgentEvent & SteeringEvent;

export function isSteeringEvent(event: AgentEvent): event is SteerEvent {
  return event.kind === 'steer_queued' || event.kind === 'steer_injected' || event.kind === 'steer_dropped';
}

function without(pending: readonly PendingSteer[] | undefined, steerId: string): PendingSteer[] {
  return (pending ?? []).filter((steer) => steer.steerId !== steerId);
}

function onQueued<V extends SteerView>(view: V, steerId: string, text: string, revision: number): V {
  const pending = view.pendingSteers ?? [];
  const index = pending.findIndex((steer) => steer.steerId === steerId);
  if (index === -1) return { ...view, pendingSteers: [...pending, { steerId, text, revision }] };
  if ((pending[index]?.revision ?? 0) >= revision) return view;
  return { ...view, pendingSteers: pending.map((steer, i) => (i === index ? { steerId, text, revision } : steer)) };
}

function onInjected<V extends SteerView>(view: V, steerId: string, text: string): V {
  const key = `s${steerId}`;
  if (view.items.some((item) => item.key === key)) return view;
  return { ...view, pendingSteers: without(view.pendingSteers, steerId), items: [...view.items, { type: 'steer', key, steerId, text }] };
}

export function applySteeringEvent<V extends SteerView>(view: V, event: SteerEvent): V {
  switch (event.kind) {
    case 'steer_queued':
      return onQueued(view, event.payload.steer_id, event.payload.text, event.payload.revision);
    case 'steer_injected':
      return onInjected(view, event.payload.steer_id, event.payload.text);
    case 'steer_dropped': {
      const { steer_id: steerId, text, reason } = event.payload;
      const next = { ...view, pendingSteers: without(view.pendingSteers, steerId) };
      if (reason !== 'turn_ended' || (view.endedSteers ?? []).some((steer) => steer.steerId === steerId)) return next;
      return { ...next, endedSteers: [...(view.endedSteers ?? []), { steerId, text }] };
    }
  }
}
