/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/
 * useSpeakingModeLoop.hooks.js` — the hands-free voice-loop orchestrator,
 * the most complex file in this cluster. Wraps BOTH ASR hooks
 * (`useStreamingSpeechRecognition`/`useSpeechRecognition`) and picks server
 * vs. client based on `serverHook.isSupported` (`!!asrModel`). Faithfully
 * ports the full derived-boolean state machine (Idle/Listening/Waiting-for-
 * AI/Restart phases), the `SILENCE_TIMEOUT_MS`=3000 + EWMA latency estimate
 * for realtime models, the `VAD_SILENCE_MS`=600 backdating for the
 * Whisper/VAD path, the manual-edit detection (comparing the input's live
 * value against `lastSetValueRef`), and `pauseForRegeneration`/
 * `notifyManualEdit`.
 *
 * `SpeakingModeInputHandle` is the injected-slot CONTRACT this hook needs
 * from whatever chat-textarea component ends up rendering the actual input
 * — same class of seam as this slice's own `lib/chatInputHandle.ts`'s
 * `ChatInputHandle` (that file's own doc comment: "this unit (C3) does not
 * own the textarea component itself — the composition-root unit (C6, build
 * last) does"). Kept as a SEPARATE interface rather than extending/reusing
 * `ChatInputHandle` — deliberately, matching that file's own stated
 * philosophy of narrow, caller-scoped imperative-handle interfaces ("never
 * any of that component's OTHER imperative methods... those belong to a
 * different caller"): `ChatInputHandle` is scoped to the two mention hooks'
 * needs (`getInputContent`/`getCursorPosition`/`replaceRange`); this hook
 * needs `getInputContent`/`getCursorPosition` (same 2, same `number | null`
 * cursor convention) PLUS `setValue`/`sendQuestion`/`reset`, which
 * `ChatInputHandle` explicitly does NOT carry. C6's real textarea component
 * implements both shapes on the same underlying ref.
 *
 * Model selection: `useModelsList({ projectId, section: 'asr', includeShared:
 * true })` (this slice's own `api/models.ts`, built by this unit — see that
 * file's module doc for the cross-cluster coordination note).
 * `useSelectedProjectId` is this slice's own local duplicate (`api/
 * useSelectedProjectId.ts`, already present in this slice from a sibling
 * cluster — reused here, not re-authored).
 */
import { useCallback, useEffect, useRef } from 'react';
import type { RefObject } from 'react';

import type { ModelListItem } from '../../api/models';
import { useModelsList } from '../../api/models';
import { useSelectedProjectId } from '../../api/useSelectedProjectId';
import type { TranscriptEvent } from './useSpeechRecognition';
import { useSpeechRecognition } from './useSpeechRecognition';
import { useStreamingSpeechRecognition } from './useStreamingSpeechRecognition';

// Realtime models: timeout after this much silence (+ EWMA latency estimate).
const SILENCE_TIMEOUT_MS = 3000;
// Added to SILENCE_TIMEOUT_MS for realtime models to cover in-flight audio
// that hasn't been transcribed yet (network round-trip + ASR model
// processing). Seeded at 500 ms; updated each turn via EWMA.
const INITIAL_LATENCY_ESTIMATE_MS = 500;
const LATENCY_EWMA_ALPHA = 0.3; // weight given to the most-recent sample
// How long the backend VAD waits (silence frames x chunk size) before
// flushing. Used to back-date speechEndedAtRef so the silence timer is
// relative to when the user actually stopped speaking, not when the
// vad_flush event arrives.
const VAD_SILENCE_MS = 600;

/**
 * Deliberately narrower than `../helpers/asrHelpers`'s exported
 * `isWhisperModel` (which also matches "transcribe") — old-app parity:
 * `useSpeakingModeLoop.hooks.js`'s own private `isWhisperModelName` is a
 * SEPARATE, narrower classifier from `asr.helpers.js`'s. N4: reproduce the
 * documented (if inconsistent) old-app behaviour rather than silently
 * unifying the two classifiers.
 */
function isWhisperModelNameForSelection(name: string | undefined): boolean {
  return Boolean(name && name.toLowerCase().includes('whisper'));
}

/** `useSpeakingModeLoop.hooks.js`'s `selectAsrModel` — prefer a default streaming model, then any streaming model, then a default whisper model, then any whisper model. */
export function selectAsrModel(items: readonly ModelListItem[]): ModelListItem | undefined {
  const streaming = items.filter((m) => !isWhisperModelNameForSelection(m.name));
  const whisper = items.filter((m) => isWhisperModelNameForSelection(m.name));
  return streaming.find((m) => m.default) ?? streaming[0] ?? whisper.find((m) => m.default) ?? whisper[0];
}

/** @public The injected-slot contract — see this file's module doc. */
export interface SpeakingModeInputHandle {
  /** The input's current full text value. */
  readonly getInputContent: () => string;
  /** The caret position in `getInputContent()`'s text, or `null` if it cannot be determined (matches `ChatInputHandle`'s own convention). */
  readonly getCursorPosition: () => number | null;
  /** Replaces the input's full value and moves the caret to `cursorPosition`. */
  readonly setValue: (value: string, cursorPosition: number) => void;
  /** Sends the input's current content as a chat message. */
  readonly sendQuestion: () => void;
  /** Clears the input after sending. */
  readonly reset: () => void;
}

export interface UseSpeakingModeLoopParams {
  readonly isSpeakingMode: boolean;
  readonly inputRef: RefObject<SpeakingModeInputHandle | null>;
  readonly isStreaming: boolean;
  readonly isTTSPlaying: boolean;
}

export interface UseSpeakingModeLoopResult {
  readonly isRecording: boolean;
  /** Called by external actions that trigger a new AI response (e.g. Regenerate) — stops recording and activates the restart guard. */
  readonly pauseForRegeneration: () => void;
  /** Called when the user manually edits the input while speaking mode is active — resets the auto-send timer so the message isn't sent mid-edit. */
  readonly notifyManualEdit: () => void;
}

const noop = (): void => {
  /* the old app passes an inline no-op for the ASR hooks' onError here too — errors surface via the mic UI's own isRecording state, not this hook's return */
};

export function useSpeakingModeLoop(params: UseSpeakingModeLoopParams): UseSpeakingModeLoopResult {
  const { isSpeakingMode, inputRef, isStreaming, isTTSPlaying } = params;
  const projectId = useSelectedProjectId();

  const preCursorRef = useRef('');
  const postCursorRef = useRef('');
  const voiceAccumulatedRef = useRef('');
  const lastSetValueRef = useRef<string | null>(null);
  const silenceTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const hasSentRef = useRef(false);
  // Tracks when the last interim transcript arrived so we can measure the
  // interim -> final round-trip time (network latency + ASR model time).
  const lastInterimTimestampRef = useRef<number | null>(null);
  // EWMA-smoothed estimate of round-trip latency in ms.
  const latencyEstimateRef = useRef(INITIAL_LATENCY_ESTIMATE_MS);
  // Holds the latest stopRecording so the silence timer can call it without
  // creating a circular declaration dependency.
  const stopRecordingRef = useRef<(() => void) | null>(null);
  // Count of vad_flush events that have not yet been matched by a
  // transcript_done. scheduleSend() only fires when this reaches 0.
  const pendingVadFlushesRef = useRef(0);
  // Timestamp (Date.now()) approximating when the user last stopped
  // speaking — see handleVadFlush.
  const speechEndedAtRef = useRef<number | null>(null);
  // #930: which ASR hook is actually driving the loop. The SERVER hook signals
  // "the utterance is over" with its own `onTranscriptDone`; the browser
  // fallback (`useSpeechRecognition`) has no such callback — a FINAL result is
  // the only end-of-utterance signal it emits. Read through a ref because the
  // hooks themselves are constructed below `handleTranscript`.
  const usesServerAsrRef = useRef(false);

  const { data: asrModelsData } = useModelsList(
    { projectId, section: 'asr', includeShared: true },
    { enabled: projectId !== undefined },
  );
  const asrModel = selectAsrModel(asrModelsData?.items ?? []);

  const clearSilenceTimer = useCallback(() => {
    if (silenceTimerRef.current) {
      clearTimeout(silenceTimerRef.current);
      silenceTimerRef.current = null;
    }
  }, []);

  // Schedule an auto-send after a silence timeout.
  // For streaming models: totalTimeout = SILENCE_TIMEOUT_MS + latencyEstimate.
  // For Whisper: called from handleTranscriptDone with a pre-computed
  // adjusted delay that accounts for elapsed time since the user actually
  // stopped speaking (supplied via overrideDelay).
  const scheduleSend = useCallback(
    (overrideDelay: number | null = null) => {
      clearSilenceTimer();
      const totalTimeout = overrideDelay !== null ? overrideDelay : SILENCE_TIMEOUT_MS + latencyEstimateRef.current;
      silenceTimerRef.current = setTimeout(() => {
        const content = inputRef.current?.getInputContent() ?? '';
        if (content.trim()) {
          hasSentRef.current = true;
          inputRef.current?.sendQuestion();
          inputRef.current?.reset();
          // Pause recording until the AI response and TTS are done.
          stopRecordingRef.current?.();
        }
      }, totalTimeout);
    },
    [clearSilenceTimer, inputRef],
  );

  // Re-sync cursor refs if the user manually edited the input between
  // transcript events (e.g. deleted the previous transcript while still
  // recording). Split out of `handleTranscript` purely to keep that
  // function under the §3.5 cyclomatic-complexity-12 budget — same logic,
  // no behavior change.
  const resyncCursorsOnManualEdit = useCallback(() => {
    if (lastSetValueRef.current === null) return;
    const currentContent = inputRef.current?.getInputContent() ?? '';
    if (currentContent === lastSetValueRef.current) return;
    const cursor = inputRef.current?.getCursorPosition() ?? currentContent.length;
    preCursorRef.current = currentContent.slice(0, cursor);
    postCursorRef.current = currentContent.slice(cursor);
    voiceAccumulatedRef.current = '';
  }, [inputRef]);

  // User is actively speaking — cancel any pending auto-send so a natural
  // pause between sentences doesn't trigger a premature send.
  const applyInterimTranscript = useCallback(
    (interim: string) => {
      clearSilenceTimer();
      lastInterimTimestampRef.current = Date.now();
      const voiceBase = preCursorRef.current + voiceAccumulatedRef.current;
      const newValue = voiceBase + interim + postCursorRef.current;
      const cursorPos = voiceBase.length + interim.length;
      lastSetValueRef.current = newValue;
      inputRef.current?.setValue(newValue, cursorPos);
    },
    [inputRef, clearSilenceTimer],
  );

  const applyFinalTranscript = useCallback(
    (final: string) => {
      // Measure interim -> final round-trip and update the EWMA latency estimate.
      if (lastInterimTimestampRef.current !== null) {
        const roundTripMs = Date.now() - lastInterimTimestampRef.current;
        latencyEstimateRef.current = LATENCY_EWMA_ALPHA * roundTripMs + (1 - LATENCY_EWMA_ALPHA) * latencyEstimateRef.current;
        lastInterimTimestampRef.current = null;
      }
      voiceAccumulatedRef.current += (voiceAccumulatedRef.current ? ' ' : '') + final;
      const newValue = preCursorRef.current + voiceAccumulatedRef.current + postCursorRef.current;
      const cursorPos = preCursorRef.current.length + voiceAccumulatedRef.current.length;
      lastSetValueRef.current = newValue;
      inputRef.current?.setValue(newValue, cursorPos);
    },
    [inputRef],
  );

  const handleTranscript = useCallback(
    ({ final, interim }: TranscriptEvent) => {
      resyncCursorsOnManualEdit();
      if (interim) applyInterimTranscript(interim);
      if (final) {
        applyFinalTranscript(final);
        // #930 (ELITEA-1294/1298/1291): arm the silence timer on the
        // browser-fallback path. On the server path `handleTranscriptDone`
        // owns this (and knows the VAD/latency adjustments); doing it here
        // too would schedule the same send twice. Without it a spoken
        // utterance on a project with no ASR model configured sat in the
        // composer forever — only a real keystroke (`notifyManualEdit`) ever
        // started the timer.
        if (!usesServerAsrRef.current) scheduleSend();
      }
    },
    [resyncCursorsOnManualEdit, applyInterimTranscript, applyFinalTranscript, scheduleSend],
  );

  // Called when the backend VAD detects the start of a new speech segment.
  const handleSpeechStarted = useCallback(() => {
    clearSilenceTimer();
  }, [clearSilenceTimer]);

  const handleVadFlush = useCallback(() => {
    pendingVadFlushesRef.current += 1;
    // Back-date by VAD_SILENCE_MS: the flush fires after the silence window,
    // so the user actually stopped speaking ~VAD_SILENCE_MS ago.
    speechEndedAtRef.current = Date.now() - VAD_SILENCE_MS;
  }, []);

  // Called on every transcript_done event (even when transcript text is empty).
  //
  // Whisper path (vad_flush fired -> speechEndedAtRef is set): decrements
  // the pending counter. When it reaches 0 (all segments done), schedules
  // auto-send with a delay adjusted so the total perceived silence from the
  // user's perspective equals SILENCE_TIMEOUT_MS.
  //
  // Realtime path (no vad_flush -> speechEndedAtRef is null):
  // pendingVadFlushesRef stays 0, falls through to the plain scheduleSend()
  // (no override) so realtime models keep using SILENCE_TIMEOUT_MS + EWMA.
  const handleTranscriptDone = useCallback(() => {
    if (pendingVadFlushesRef.current > 0) {
      pendingVadFlushesRef.current -= 1;
    }
    if (pendingVadFlushesRef.current === 0) {
      if (speechEndedAtRef.current !== null) {
        const elapsed = Math.max(0, Date.now() - speechEndedAtRef.current);
        speechEndedAtRef.current = null;
        const adjustedDelay = Math.max(200, SILENCE_TIMEOUT_MS - elapsed);
        scheduleSend(adjustedDelay);
      } else {
        scheduleSend();
      }
    }
  }, [scheduleSend]);

  // Called when the user manually edits the input field while speaking mode
  // is active. Resets the auto-send timer so the message isn't sent mid-edit.
  const notifyManualEdit = useCallback(() => {
    if (!isSpeakingMode || hasSentRef.current) return;
    scheduleSend(null);
  }, [isSpeakingMode, scheduleSend]);

  const serverHook = useStreamingSpeechRecognition({
    onTranscript: handleTranscript,
    onTranscriptDone: handleTranscriptDone,
    onSpeechStarted: handleSpeechStarted,
    onVadFlush: handleVadFlush,
    onError: noop,
    projectId,
    asrModel,
  });

  const clientHook = useSpeechRecognition({
    onTranscript: handleTranscript,
    onError: noop,
  });

  const { isRecording, isSupported, startRecording, stopRecording } = serverHook.isSupported ? serverHook : clientHook;

  // Keep the ref in sync so the silence timer always calls the latest version.
  stopRecordingRef.current = stopRecording;
  usesServerAsrRef.current = serverHook.isSupported;

  const beginRecording = useCallback(() => {
    preCursorRef.current = '';
    postCursorRef.current = '';
    voiceAccumulatedRef.current = '';
    lastSetValueRef.current = '';
    pendingVadFlushesRef.current = 0;
    speechEndedAtRef.current = null;
    // `startRecording` is `(() => Promise<void>) | (() => void)` — the
    // union of both ASR hooks' signatures (TS cannot narrow a ternary
    // picked at runtime by a plain boolean). No `void` operator here: it
    // resolves to plain `void`, not a floating promise.
    startRecording();
  }, [startRecording]);

  // Start/stop when speaking mode toggles.
  useEffect(() => {
    if (isSpeakingMode && isSupported) {
      beginRecording();
    } else if (!isSpeakingMode) {
      clearSilenceTimer();
      stopRecording();
    }
    // oxlint-disable-next-line react/exhaustive-deps -- old-app parity (useSpeakingModeLoop.hooks.js's "Start/stop when speaking mode toggles" effect): fires only on isSpeakingMode changing, not on every identity change of isSupported/beginRecording/stopRecording/clearSilenceTimer.
  }, [isSpeakingMode]);

  // Manage recording around AI responses:
  // - streaming or TTS active -> stop recording, cancel pending send.
  // - both finished -> restart recording for the next turn.
  useEffect(() => {
    if (!isSpeakingMode) return;
    if (isStreaming || isTTSPlaying) {
      clearSilenceTimer();
      stopRecordingRef.current?.();
      hasSentRef.current = true;
    } else if (hasSentRef.current) {
      hasSentRef.current = false;
      beginRecording();
    }
    // oxlint-disable-next-line react/exhaustive-deps -- old-app parity (useSpeakingModeLoop.hooks.js's "Manage recording around AI responses" effect): deliberately scoped to isStreaming/isTTSPlaying/isSpeakingMode only.
  }, [isStreaming, isTTSPlaying, isSpeakingMode]);

  // Called by external actions that trigger a new AI response (e.g.
  // Regenerate). Stops recording and activates the restart guard, identical
  // to the auto-send path.
  const pauseForRegeneration = useCallback(() => {
    if (!isSpeakingMode) return;
    clearSilenceTimer();
    stopRecordingRef.current?.();
    hasSentRef.current = true;
  }, [isSpeakingMode, clearSilenceTimer]);

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      clearSilenceTimer();
      stopRecording();
    };
    // oxlint-disable-next-line react/exhaustive-deps -- unmount-only cleanup, old-app parity (useSpeakingModeLoop.hooks.js's final cleanup effect, empty dep array).
  }, []);

  return { isRecording, pauseForRegeneration, notifyManualEdit };
}
