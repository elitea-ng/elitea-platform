import { useEffect, useState } from 'react';

import { fetchGuardrailNotices, type GuardrailNotice } from '@/api/queries/guardrailNotices';
import type { LiveTurn } from '@/features/chat/turnController';

/** The notices of the latest live turn, once it has ended. */
export interface TurnGuardrailNotices {
  readonly runId: string;
  readonly notices: readonly GuardrailNotice[];
}

/**
 * Reads the guardrail notices of the live turn when it ends (an answer, a
 * failure), once per run. A thread has no stored link from an answer to its
 * run, so the notices belong to the turn the person just watched: they stay
 * until the next turn starts. A turn that paused, or was handed to the outbox,
 * has none yet.
 */
export function useGuardrailNotices(projectId: number | null, live: LiveTurn | null): TurnGuardrailNotices | null {
  const [found, setFound] = useState<TurnGuardrailNotices | null>(null);
  const runId = live?.execution?.executionId ?? null;
  const ended = live !== null && !live.queued && (live.turn.phase === 'done' || live.turn.phase === 'failed');

  useEffect(() => {
    if (projectId === null || runId === null || !ended) return undefined;
    const controller = new AbortController();
    void fetchGuardrailNotices(projectId, runId, undefined, controller.signal).then((notices) => {
      if (!controller.signal.aborted) setFound({ runId, notices });
    });
    return () => controller.abort();
  }, [projectId, runId, ended]);

  // A new run replaces the previous run's notices at once.
  if (runId !== null && found !== null && found.runId !== runId) return null;
  return found !== null && found.notices.length > 0 ? found : null;
}
