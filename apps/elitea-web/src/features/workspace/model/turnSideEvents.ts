/**
 * The turn events that are not the transcript's main line, folded into the
 * turn's view (`TurnView`): the context meter and the turn's tokens (`usage`,
 * `usage_total`, `context`, #1227) and a model step's reasoning
 * (`thinking_delta`, `thinking_end`, issue 1228), a model call's retry
 * (`model_retry`, `modelRetry.ts`) and a `run_command` row's background
 * process (`processEvents.ts`, #1223), and the steering queue
 * (`steeringEvents.ts`).
 */
import type { AgentEvent, ContextMeter } from '@/shared/desktop/workspaceIpc';

import { onModelRetry } from './modelRetry';
import { applyProcessEvent, isProcessEvent, type ProcessEvent } from './processEvents';
import { applySteeringEvent, isSteeringEvent, type SteerView } from './steeringEvents';
import type { TranscriptItem } from './transcriptItem';
import { addCall, settle, type TurnUsage } from './turnUsage';

/** The part of a turn's view these events change: its transcript, its meter, its usage and its steering queue. */
interface SideView extends SteerView {
  items: TranscriptItem[];
  context?: ContextMeter | undefined;
  usage?: TurnUsage | undefined;
}

type EventOf<K extends AgentEvent['kind']> = Extract<AgentEvent, { kind: K }>;

type MeterEvent = EventOf<'usage'> | EventOf<'usage_total'> | EventOf<'context'>;
type ThinkingEvent = EventOf<'thinking_delta'> | EventOf<'thinking_end'>;
type SideEvent = MeterEvent | ThinkingEvent | EventOf<'model_retry'> | ProcessEvent | Parameters<typeof applySteeringEvent>[1];

/** `usage` (a call that ended, with its meter), `usage_total` (the run ended) or `context` (a phase change, sent at once). */
function isMeterEvent(event: AgentEvent): event is MeterEvent {
  return event.kind === 'usage' || event.kind === 'usage_total' || event.kind === 'context';
}

/**
 * Not part of the transcript: the composer's usage chip shows the turn's
 * tokens (summed per call, then the host's total), its meter the latest
 * context occupancy, which a `context` event carries while a summary call runs.
 */
function onMeter<V extends SideView>(view: V, event: MeterEvent): V {
  if (event.kind === 'usage_total') return { ...view, usage: settle(view.usage, event.payload) };
  if (event.kind === 'context') return { ...view, context: event.payload };
  const counted = { ...view, usage: addCall(view.usage, event.payload) };
  return event.payload.context === undefined ? counted : { ...counted, context: event.payload.context };
}

/** Reasoning streams into the step's open "Thinking" block; `thinking_end` closes it. */
function onThinking<V extends SideView>(view: V, event: ThinkingEvent): V {
  const last = view.items[view.items.length - 1];
  const open = last?.type === 'thinking' && last.chars === undefined ? last : undefined;
  if (event.kind === 'thinking_end') {
    return open === undefined ? view : { ...view, items: [...view.items.slice(0, -1), { ...open, chars: event.payload.chars }] };
  }
  if (open !== undefined) return { ...view, items: [...view.items.slice(0, -1), { ...open, text: open.text + event.payload.text }] };
  return { ...view, items: [...view.items, { type: 'thinking', key: `r${event.seq}`, text: event.payload.text }] };
}

function isThinkingEvent(event: AgentEvent): event is ThinkingEvent {
  return event.kind === 'thinking_delta' || event.kind === 'thinking_end';
}

export function isSideEvent(event: AgentEvent): event is SideEvent {
  return isMeterEvent(event) || isThinkingEvent(event) || event.kind === 'model_retry' || isProcessEvent(event) || isSteeringEvent(event);
}

export function applySideEvent<V extends SideView>(view: V, event: SideEvent): V {
  if (isProcessEvent(event)) return applyProcessEvent(view, event);
  if (isSteeringEvent(event)) return applySteeringEvent(view, event);
  if (event.kind === 'model_retry') return onModelRetry(view, event);
  return isMeterEvent(event) ? onMeter(view, event) : onThinking(view, event);
}
