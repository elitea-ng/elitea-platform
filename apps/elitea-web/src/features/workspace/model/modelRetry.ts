/**
 * `model_retry` folded into a turn's view: the host makes a failed model call
 * again (a cut or stalled stream, the gateway unreachable, a 408, 409, 429 or
 * 5xx; at most 25 times). What the failed call streamed comes off the end of
 * the transcript, and the retry shows until the model streams again or the
 * turn ends.
 */
import type { AgentEvent, ModelRetryPayload } from '@/shared/desktop/workspaceIpc';

import type { TranscriptItem } from './transcriptItem';

export type RetryingView = Pick<ModelRetryPayload, 'attempt' | 'max_attempts' | 'message'>;

/** The part of a turn's view a retry changes. */
interface RetryView {
  items: TranscriptItem[];
  retrying?: RetryingView | undefined;
}

type RetryEvent = Extract<AgentEvent, { kind: 'model_retry' }>;

/** The failed call's text, then its reasoning (UTF-16 code units, like a JS string), cut off the end; never past a tool row. */
export function onModelRetry<V extends RetryView>(view: V, event: RetryEvent): V {
  const { attempt, max_attempts: maxAttempts, message } = event.payload;
  let text = event.payload.text_units;
  let thinking = event.payload.thinking_units;
  const items = [...view.items];
  while (text > 0 || thinking > 0) {
    const last = items[items.length - 1];
    if (last === undefined || last.type === 'tool') break;
    const left = last.type === 'text' ? text : thinking;
    if (left <= 0) break;
    const cut = Math.min(left, last.text.length);
    if (last.type === 'text') text -= cut;
    else thinking -= cut;
    const kept = last.text.slice(0, last.text.length - cut);
    if (kept === '') items.pop();
    else items[items.length - 1] = { ...last, text: kept };
  }
  return { ...view, items, retrying: { attempt, max_attempts: maxAttempts, message } };
}

/** The model streams again, or the turn ended: no retry is waiting any more. */
export function withoutRetry<V extends RetryView>(view: V): V {
  if (view.retrying === undefined) return view;
  const { retrying: _done, ...rest } = view;
  return rest as V;
}

const ENDS_RETRY: ReadonlySet<AgentEvent['kind']> = new Set(['text_delta', 'thinking_delta', 'tool_call', 'usage', 'error', 'done']);

/** The events that show the retried call answered, or that the turn moved on. */
export function endsRetry(event: AgentEvent): boolean {
  if (event.kind === 'status') return event.payload.phase === 'done' || event.payload.phase === 'cancelled' || event.payload.phase === 'error';
  return ENDS_RETRY.has(event.kind);
}
