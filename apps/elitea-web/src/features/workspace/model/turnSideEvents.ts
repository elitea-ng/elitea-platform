/**
 * The turn events that are not the transcript's main line, folded into the
 * `TurnView`: the context meter (`usage`, `context`) and a model step's
 * reasoning (`thinking_delta`, `thinking_end`, issue 1228).
 */
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import type { TurnView } from './turnReducer';

type EventOf<K extends AgentEvent['kind']> = Extract<AgentEvent, { kind: K }>;

type MeterEvent = EventOf<'usage'> | EventOf<'context'>;
type ThinkingEvent = EventOf<'thinking_delta'> | EventOf<'thinking_end'>;
type SideEvent = MeterEvent | ThinkingEvent;

/** `usage` (the meter of a call that ended) or `context` (a phase change, sent at once). */
function isMeterEvent(event: AgentEvent): event is MeterEvent {
  return event.kind === 'usage' || event.kind === 'context';
}

/**
 * Not part of the transcript: the activity island shows a turn's tokens; the
 * composer's meter the latest context occupancy, which a `context` event
 * carries while a summary call runs.
 */
function onMeter(view: TurnView, event: MeterEvent): TurnView {
  const context = event.kind === 'context' ? event.payload : event.payload.context;
  return context === undefined ? view : { ...view, context };
}

/** Reasoning streams into the step's open "Thinking" block; `thinking_end` closes it. */
function onThinking(view: TurnView, event: ThinkingEvent): TurnView {
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
  return isMeterEvent(event) || isThinkingEvent(event);
}

export function applySideEvent(view: TurnView, event: SideEvent): TurnView {
  return isMeterEvent(event) ? onMeter(view, event) : onThinking(view, event);
}
