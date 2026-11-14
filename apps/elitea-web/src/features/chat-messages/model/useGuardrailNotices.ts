/**
 * The guardrail notices of one settled answer (issue 2012).
 *
 * Asked once per answer, after the turn ends, and only for an answer that can
 * have one: a live answer (it carries the run id the stream followed), or a
 * failed answer whose code a gateway refusal arrives as (`MODEL_REQUEST_REJECTED`
 * for a block, `MODEL_UNAVAILABLE` for a closed guardrail that could not run).
 * A transcript of old answers never fans a request out per row.
 *
 * Plain state and an effect, not react-query: the answer renders in trees that
 * have no query client (shared chats, the playback view, most unit tests), and
 * a notice is a courtesy that must never break the answer it sits under.
 */
import { useEffect, useState } from 'react';

import type { RunGuardrailNotice } from '@/shared/api/generated/model';

import { fetchGuardrailNotices } from '../api/guardrailNoticesApi';

/** The failure codes a gateway guardrail refusal reaches the chat as. */
const GUARDRAIL_FAILURE_CODES: ReadonlySet<string> = new Set(['MODEL_REQUEST_REJECTED', 'MODEL_UNAVAILABLE']);

const NONE: readonly RunGuardrailNotice[] = [];

/**
 * Notices found, so a remount does not ask again. Bounded: oldest out. Only a
 * non-empty answer is remembered for good: an empty list (or a failed read) may
 * be a verdict that was not written yet, so it is remembered only for
 * `EMPTY_TTL_MS`, after which the answer is asked about again.
 */
const CACHE_LIMIT = 200;
const EMPTY_TTL_MS = 30_000;
/** One read after the settle delay, and one more when it came back empty. */
const ATTEMPTS = 2;
const cache = new Map<string, readonly RunGuardrailNotice[]>();
const emptyAt = new Map<string, number>();

function remember(key: string, notices: readonly RunGuardrailNotice[]): void {
  if (notices.length === 0) {
    emptyAt.set(key, Date.now());
    return;
  }
  if (cache.size >= CACHE_LIMIT) {
    const oldest = cache.keys().next();
    if (!oldest.done) cache.delete(oldest.value);
  }
  cache.set(key, notices);
}

function knownEmpty(key: string): boolean {
  const at = emptyAt.get(key);
  if (at === undefined) return false;
  if (Date.now() - at < EMPTY_TTL_MS) return true;
  emptyAt.delete(key);
  return false;
}

export interface GuardrailNoticeSource {
  readonly projectId: string | undefined;
  /** The run the answer came from: `executionId` of a live answer, else the stored `taskId`. */
  readonly runId: string | undefined;
  /** The turn has ended: nothing is streaming or loading. */
  readonly settled: boolean;
  /** The answer carries an error. */
  readonly failed: boolean;
  readonly failureCode: string | undefined;
  /** The answer streamed in this session. */
  readonly live: boolean;
}

export function mayHaveGuardrailNotice(source: GuardrailNoticeSource): boolean {
  if (!source.settled || !source.projectId || !source.runId) return false;
  if (source.failed) return source.failureCode !== undefined && GUARDRAIL_FAILURE_CODES.has(source.failureCode);
  return source.live;
}

/** Test seam: forget every remembered answer. */
export function resetGuardrailNoticeCache(): void {
  cache.clear();
  emptyAt.clear();
}

export function useGuardrailNotices(source: GuardrailNoticeSource): readonly RunGuardrailNotice[] {
  const enabled = mayHaveGuardrailNotice(source);
  const projectId = source.projectId ?? '';
  const runId = source.runId ?? '';
  const key = `${projectId}:${runId}`;
  const [found, setFound] = useState<{ readonly key: string; readonly notices: readonly RunGuardrailNotice[] } | undefined>(undefined);

  useEffect(() => {
    if (!enabled || cache.has(key) || knownEmpty(key)) return undefined;
    const controller = new AbortController();
    void (async () => {
      let notices: readonly RunGuardrailNotice[] = NONE;
      for (let attempt = 0; attempt < ATTEMPTS && notices.length === 0; attempt += 1) {
        notices = await fetchGuardrailNotices(projectId, runId, undefined, controller.signal);
        if (controller.signal.aborted) return;
      }
      remember(key, notices);
      setFound({ key, notices });
    })();
    return () => { controller.abort(); };
  }, [enabled, key, projectId, runId]);

  if (!enabled) return NONE;
  return cache.get(key) ?? (found?.key === key ? found.notices : NONE);
}
