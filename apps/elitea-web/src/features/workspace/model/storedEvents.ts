/**
 * The events a turn keeps (`turnReducer`): sorted by `seq`, unique, and
 * compact. A `text_delta` (or `thinking_delta`) whose seq directly follows a
 * stored run of its kind is merged into it (the run then covers `seq..last`).
 */
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

/** A stored event; a merged text run covers the seqs `seq..last`, each seen. */
export type StoredEvent = AgentEvent & { last: number };

type DeltaEvent = Extract<AgentEvent, { kind: 'text_delta' | 'thinking_delta' }>;

function lastSeq(event: AgentEvent | StoredEvent): number {
  return 'last' in event ? event.last : event.seq;
}

const isDelta = (event: AgentEvent | undefined): event is DeltaEvent => event?.kind === 'text_delta' || event?.kind === 'thinking_delta';

/** `event` merged onto `run` when it is a delta (or run) of the run's kind directly following it; otherwise null. */
function extendRun(run: StoredEvent | undefined, event: AgentEvent | StoredEvent): StoredEvent | null {
  if (run === undefined || !isDelta(run) || !isDelta(event) || run.kind !== event.kind || event.seq !== run.last + 1) return null;
  return { ...run, payload: { text: run.payload.text + event.payload.text }, last: lastSeq(event) };
}

/** Sorted events (unique seqs) as stored: contiguous text deltas merged into runs. */
export function compact(events: readonly (AgentEvent | StoredEvent)[]): StoredEvent[] {
  const out: StoredEvent[] = [];
  for (const event of events) {
    const merged = extendRun(out[out.length - 1], event);
    if (merged === null) out.push({ ...event, last: lastSeq(event) });
    else out[out.length - 1] = merged;
  }
  return out;
}

/** `events` with `event` (a higher seq than any) appended, merged into the last run when it extends it. */
export function appended(events: readonly StoredEvent[], event: AgentEvent): StoredEvent[] {
  const merged = extendRun(events[events.length - 1], event);
  return merged === null ? [...events, { ...event, last: event.seq }] : [...events.slice(0, -1), merged];
}
