/**
 * What stands above each turn of a thread with an unattended chain (ADR-0045):
 * the person's prompt, or for a turn that the host continued by itself, the
 * marker in its place (`TurnTranscript`'s `continuation`). Such a turn's
 * prompt is the host's, not the person's.
 */
import type { WorkspaceTurn } from '@/features/workspace';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

/** A recorded turn (`thread_history`) that the host started by itself: its prompt is the host's, not the person's. */
export function isContinuedRecord(events: readonly AgentEvent[]): boolean {
  return events.some((event) => event.kind === 'status' && event.payload.continued_automatically === true);
}

/** Above a turn of a thread: the index of the person's prompt that started it, or the host's continuation number. */
export type TurnSlot = { prompt: number } | { continuation: number };

/**
 * What stands above each of a thread's turns (oldest first): the person's next prompt, or for a turn that the host
 * continued by itself, its continuation number. A turn counts as continued once the turn before it says so
 * (`turn_continued`), before its own `status` arrives.
 */
export function turnSlots(views: readonly { turnId: string; view: Pick<WorkspaceTurn['view'], 'continuation' | 'continuedTo'> }[]): TurnSlot[] {
  let next = 0;
  return views.map((entry, index) => {
    const before = views[index - 1]?.view.continuedTo;
    const continuation = entry.view.continuation ?? (before?.nextTurnId === entry.turnId ? before.index : undefined);
    if (continuation !== undefined) return { continuation };
    next += 1;
    return { prompt: next - 1 };
  });
}
