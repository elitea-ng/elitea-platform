/**
 * What stands above each turn of a thread with an unattended chain (ADR-0045):
 * the person's prompt, or for a turn that the host continued by itself, the
 * marker in its place (`TurnTranscript`'s `continuation`). Such a turn's
 * prompt is the host's, not the person's.
 *
 * A later agent of a message addressed to several (ADR-0050 decision 7f) answers the person's message above it: it
 * takes no prompt of its own and shows no chain marker (`{ floor }`, the agent's name).
 */
import type { WorkspaceTurn } from '@/features/workspace';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

/** A recorded turn (`thread_history`) that the host started by itself: its prompt is the host's, not the person's. */
export function isContinuedRecord(events: readonly AgentEvent[]): boolean {
  return events.some((event) => event.kind === 'status' && (event.payload.continued_automatically === true || (event.payload.floor?.position ?? 0) > 0));
}

/**
 * Above a turn of a thread: the index of the person's prompt that started it, the host's continuation number, or
 * `resumed` for a turn that Continue started after a restart (no prompt of the person, no chain number).
 */
export type TurnSlot = { prompt: number } | { continuation: number } | { resumed: true } | { floor: string };

/**
 * What stands above each of a thread's turns (oldest first): the person's next prompt, or for a turn that the host
 * continued by itself, its continuation number. A turn counts as continued once the turn before it says so
 * (`turn_continued`), before its own `status` arrives.
 */
type SlotView = Pick<WorkspaceTurn['view'], 'continuation' | 'continuedTo' | 'resumed' | 'agent'>;

/** A later agent of a message addressed to several: its name, known from its own status or from the turn before it. */
function laterAgent(entry: { turnId: string; view: SlotView }, before: SlotView['continuedTo']): string | undefined {
  const named = before?.kind === 'floor' && before.nextTurnId === entry.turnId;
  if (!named && (entry.view.agent?.position ?? 0) === 0) return undefined;
  return entry.view.agent?.name ?? before?.agentName ?? '';
}

export function turnSlots(views: readonly { turnId: string; view: SlotView }[]): TurnSlot[] {
  let next = 0;
  return views.map((entry, index) => {
    const before = views[index - 1]?.view.continuedTo;
    const floor = laterAgent(entry, before);
    if (floor !== undefined) return { floor };
    const continuation = entry.view.continuation ?? (before?.nextTurnId === entry.turnId ? before.index : undefined);
    if (continuation !== undefined) return { continuation };
    if (entry.view.resumed === true) return { resumed: true };
    next += 1;
    return { prompt: next - 1 };
  });
}
