/**
 * RAF highlight-loop math for the model TTS engine
 * (`useTextToSpeech.hooks.js:650-767`) — split out (a pure function over
 * `ModelTtsRefs`, not a hook itself) so `useModelTtsEngine.hooks.ts`'s own
 * RAF-driving `useEffect` stays a thin dispatcher under the §3.5
 * complexity-12 budget; see `ttsHighlight.helpers.ts` for the underlying
 * interpolation math this delegates to.
 */
import { charPosFromTimeline, charPosFromWaypoints, wordRangeAround } from '../helpers/ttsHighlight.helpers';

import type { ModelTtsRefs } from './useModelTtsEngine.types';

export type ModelTickOutcome =
  /** AudioContext not yet created, or created but no chunk has started playing yet — keep polling. */
  | { readonly kind: 'wait' }
  /** `stopModelAudio` already closed the context — exit the RAF loop entirely. */
  | { readonly kind: 'closed' }
  /** All audio received and (already, or now) fully played — transition to `done`. */
  | { readonly kind: 'done' }
  /** Still playing; `spokenRange` is the word to highlight this frame, `null` when nothing new to report. */
  | { readonly kind: 'progress'; readonly spokenRange: { readonly start: number; readonly end: number } | null };

function computeSpokenRange(refs: ModelTtsRefs, elapsed: number, text: string, allReceived: boolean, totalDuration: number): { readonly start: number; readonly end: number } | null {
  const waypoints = refs.sentenceWaypoints.current;
  // While more audio can still come, nothing is heard past the end of the
  // scheduled audio: the next sentence's request has not returned yet. Hold
  // the highlight there instead of extrapolating into text not yet spoken.
  const scheduledEnd = refs.playStartTime.current !== null ? refs.nextStartTime.current - refs.playStartTime.current : elapsed;
  const heard = allReceived ? elapsed : Math.min(elapsed, scheduledEnd);
  const charPos =
    waypoints.length > 0
      ? charPosFromWaypoints(heard, waypoints, text.length, allReceived, totalDuration, refs.calibratedRate.current)
      : charPosFromTimeline(elapsed, refs.charTimeline.current, allReceived, totalDuration, refs.calibratedRate.current, text.length);
  return wordRangeAround(text, charPos);
}

/**
 * Cancels whatever frame is currently scheduled, if any. Split out (an
 * imported function, not inline) so `useModelTtsEngine.hooks.ts`'s RAF
 * effect's cleanup calls this instead of reading `refs.raf.current` as a
 * direct member access in the cleanup closure body itself — the ref here is
 * intentionally re-read fresh at cleanup time (we want to cancel whichever
 * frame id is CURRENT, not a stale one captured at effect-setup time; this
 * is manually-managed engine state, not a React-owned DOM ref), which the
 * `react-hooks/exhaustive-deps` "ref value read in cleanup" heuristic can't
 * distinguish from the DOM-ref staleness bug it exists to catch.
 */
export function cancelScheduledFrame(refs: ModelTtsRefs): void {
  if (refs.raf.current !== null) {
    cancelAnimationFrame(refs.raf.current);
    refs.raf.current = null;
  }
}

/** One RAF-tick's worth of decision-making: what changed, and whether the loop should keep going. Reads `AudioContext.currentTime`/`outputLatency` (so the highlight tracks what the user actually hears, not what has been sent to the hardware buffer) plus the refs bag's playback bookkeeping — no state mutation of its own. */
export function computeModelTickOutcome(refs: ModelTtsRefs): ModelTickOutcome {
  const ctx = refs.audioContext.current;
  if (!ctx) return { kind: 'wait' };
  if (ctx.state === 'closed') return { kind: 'closed' };

  if (refs.playStartTime.current === null) {
    // tts_done fired before any audio chunk arrived (empty response) — complete immediately rather than spinning forever.
    return refs.allChunksReceived.current ? { kind: 'done' } : { kind: 'wait' };
  }

  const elapsed = ctx.currentTime - (ctx.outputLatency ?? 0) - refs.playStartTime.current;
  const text = refs.fullText.current;
  const totalDuration = refs.totalDuration.current;
  const allReceived = refs.allChunksReceived.current;

  const spokenRange = elapsed >= 0 && text ? computeSpokenRange(refs, elapsed, text, allReceived, totalDuration) : null;

  if (allReceived && (totalDuration <= 0 || elapsed >= totalDuration)) return { kind: 'done' };
  return { kind: 'progress', spokenRange };
}
