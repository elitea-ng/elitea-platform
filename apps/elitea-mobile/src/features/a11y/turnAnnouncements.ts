import { useEffect, useRef } from 'react';

import { createAnnouncer, type Announcer } from '@/features/a11y/announce';
import type { LiveTurn } from '@/features/chat/turnController';
import type { TurnState } from '@/features/chat/turnReducer';

/**
 * What a screen reader hears when a live turn changes phase: once when the
 * answer is done (or stopped, or failed), once when it pauses for the user.
 * Streaming deltas are never announced.
 */
export function turnAnnouncement(
  before: Pick<TurnState, 'phase'> | null,
  after: Pick<TurnState, 'phase' | 'stopped' | 'pause' | 'error'>,
  context: { author: string | null; regeneration: boolean },
): string | null {
  if (before?.phase === after.phase) return null;
  switch (after.phase) {
    case 'done':
      if (after.stopped) return 'Answer stopped.';
      if (context.regeneration) return 'New answer ready.';
      return context.author ? `${context.author} answered.` : 'Answer ready.';
    case 'paused':
      if (after.pause?.kind === 'auth') return 'A toolkit needs you to sign in before the agent can go on.';
      if (after.pause?.kind === 'hitl') {
        const question = after.pause.interrupts.some((i) => i.questions.length > 0);
        if (question) return 'The agent is asking you a question.';
        const tool = after.pause.interrupts.find((i) => i.toolName)?.toolName;
        return tool ? `Approval needed to run ${tool}.` : 'The agent is waiting for your approval.';
      }
      return 'The agent is waiting for you.';
    case 'failed':
      return after.error?.message ? `The answer failed. ${after.error.message}` : 'The answer failed.';
    default:
      return null;
  }
}

/**
 * Announces the live turn's phase changes (debounced), for the thread screen. `enabled` false
 * keeps it quiet (the phase is still tracked): the detail column beside the Chats list while
 * another screen is in front, which may be the same chat on its own screen (issue #15), so a
 * screen reader hears an answer once.
 */
export function useTurnAnnouncements(live: LiveTurn | null, author: string | null, enabled = true): void {
  const announcer = useRef<Announcer | null>(null);
  const previous = useRef<{ questionId: string; phase: TurnState['phase'] } | null>(null);
  useEffect(() => {
    announcer.current = createAnnouncer();
    return () => announcer.current?.cancel();
  }, []);
  const phase = live?.turn.phase ?? null;
  useEffect(() => {
    if (live === null) {
      previous.current = null;
      return;
    }
    const before = previous.current?.questionId === live.questionId ? { phase: previous.current.phase } : null;
    previous.current = { questionId: live.questionId, phase: live.turn.phase };
    // A turn found already finished on mount (re-attached) is not news.
    if (before === null && live.turn.phase !== 'sending' && live.turn.phase !== 'streaming') return;
    const text = turnAnnouncement(before, live.turn, { author, regeneration: live.regeneration !== null });
    if (text !== null && enabled) announcer.current?.say(text);
    // Phase changes only; the live object changes on every delta.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live?.questionId, phase]);
}
