/**
 * The server-side model TTS backend (HTTPS + Web Audio) — ported from
 * `useTextToSpeech.hooks.js`'s `hasModelTTS` branch (lines 98-644). Split
 * across `.types.ts` (the shared refs bag), `.scheduler.ts` (AudioContext +
 * buffered PCM queue), `.stream.ts` (one `POST /llm/v1/audio/speech` per
 * sentence — it replaced the socket.io `tts_*` events, see
 * `api/voiceTransport.ts`), and `.raf.ts` (highlight-loop math) — this file
 * wires those pieces into React effects/callbacks and stays a thin
 * dispatcher, per those files' own module docs (§3.5 budgets).
 *
 * State ownership split from the baseline (deliberate, not a behavioral
 * change): the baseline's `useTextToSpeech` owns `status`/`spokenRange`
 * directly as component state; here the OUTER `useTextToSpeech.hooks.ts`
 * owns that state (shared with the browser engine, only one of which is
 * ever active) and this engine receives the current `status` PLUS a
 * `setStatus` setter — same as the baseline's own `pause`/`resume`
 * `useCallback(..., [status, hasModelTTS])` dependency, just sourced from a
 * parent instead of local component state.
 */
import { useCallback, useEffect, useRef } from 'react';

import { isVoiceAbort, VoiceTransportError } from '../../api/voiceTransport';
import type { SpeechRequest, VoiceErrorCode } from '../../api/voiceTransport';
import { buildCharTimeline } from '../helpers/ttsTimeline.helpers';
import { speechInstructionsFor } from '../helpers/voiceAudio.helpers';

import { cancelScheduledFrame, computeModelTickOutcome } from './useModelTtsEngine.raf';
import { ensureAudioContext, scheduleFromQueue, stopModelAudio } from './useModelTtsEngine.scheduler';
import { streamSpeech } from './useModelTtsEngine.stream';
import type { ModelTtsRefs } from './useModelTtsEngine.types';
import type { TtsEngineHandle, TtsModel, TtsSpokenRange, TtsStatus, TtsVoiceConfig } from './useTextToSpeech.types';

/**
 * Every `ModelTtsRefs` member, in one `useRef`-per-field factory. Each
 * individual `useRef()` call is unconditional (rules-of-hooks-safe) and
 * already returns a stable object across renders; wrapping the whole BAG in
 * its own outer `useRef` (built exactly once, on the first render) gives
 * the returned `ModelTtsRefs` object itself a stable identity too, so
 * `refs` is safe to list in a `useCallback`/`useEffect` dependency array
 * without defeating memoization.
 */
function useModelTtsRefs(): ModelTtsRefs {
  const audioContext = useRef<AudioContext | null>(null);
  const masterGain = useRef<GainNode | null>(null);
  const nextStartTime = useRef(0);
  const scheduledSources = useRef<AudioBufferSourceNode[]>([]);
  const playStartTime = useRef<number | null>(null);
  const totalDuration = useRef(0);
  const allChunksReceived = useRef(false);
  const userPaused = useRef(false);
  const calibratedRate = useRef(15.4);
  const charTimeline: ModelTtsRefs['charTimeline'] = useRef(null);
  const sentenceWaypoints: ModelTtsRefs['sentenceWaypoints'] = useRef([]);
  const pcmQueue: ModelTtsRefs['pcmQueue'] = useRef([]);
  const schedulerTimer: ModelTtsRefs['schedulerTimer'] = useRef(null);
  const finalTtsDone = useRef(false);
  const totalEnqueuedSamples = useRef(0);
  const sampleRate = useRef(24000);
  const fullText = useRef('');
  const raf = useRef<number | null>(null);

  const bagRef = useRef<ModelTtsRefs | null>(null);
  bagRef.current ??= {
    audioContext,
    masterGain,
    nextStartTime,
    scheduledSources,
    playStartTime,
    totalDuration,
    allChunksReceived,
    userPaused,
    calibratedRate,
    charTimeline,
    sentenceWaypoints,
    pcmQueue,
    schedulerTimer,
    finalTtsDone,
    totalEnqueuedSamples,
    sampleRate,
    fullText,
    raf,
  };
  return bagRef.current;
}

export interface UseModelTtsEngineParams {
  /** `hasModelTTS` — `false` keeps every effect below a no-op (this engine idle while the browser engine is active). */
  readonly enabled: boolean;
  /** The outer hook's current status — this engine only ever transitions it away from `playing`/`paused` states it itself set. */
  readonly status: TtsStatus;
  readonly ttsModel: TtsModel | null | undefined;
  /** The project the user works in: the `/llm` edge bills it (`X-Project-Id`). */
  readonly projectId: string | number | undefined;
  readonly voiceConfig: TtsVoiceConfig | undefined;
  readonly setStatus: (status: TtsStatus) => void;
  readonly setSpokenRange: (range: TtsSpokenRange | null) => void;
  /** Called when playback reaches `done` (RAF loop), `error` (a failed speech request), or is explicitly `stop()`-ed (`'idle'`) — the outer hook resets `showPlayer`/`speakableText` here in every case, matching baseline's unconditional `resetStatus(newStatus)`. */
  readonly onFinished: (status: 'done' | 'error' | 'idle') => void;
  /** Why a speech request failed, for the caller to show. A stop is not a failure. */
  readonly onError?: ((code: VoiceErrorCode) => void) | undefined;
}

/** The voice the request names: the user's pick, else `alloy` (the old server's default, `sio/tts.py`). */
const DEFAULT_SPEECH_VOICE = 'alloy';

/** Everything one speech request carries except the sentence itself. */
function speechRequestFor(model: TtsModel, projectId: string | number, voiceConfig: TtsVoiceConfig | undefined): Omit<SpeechRequest, 'input'> {
  return {
    projectId,
    model: model.name,
    voice: voiceConfig?.voiceId || DEFAULT_SPEECH_VOICE,
    speed: voiceConfig?.rate ?? 1.0,
    instructions: speechInstructionsFor(model.name),
  };
}

export function useModelTtsEngine(params: UseModelTtsEngineParams): TtsEngineHandle {
  const { enabled, status, ttsModel, projectId, voiceConfig, setStatus, setSpokenRange, onFinished, onError } = params;
  const refs = useModelTtsRefs();
  // One run at a time: a new speak() or a stop() aborts the run in flight.
  const runRef = useRef<AbortController | null>(null);

  // Live volume change — ramp to avoid a click artifact when the volume slider moves mid-playback.
  useEffect(() => {
    const ctx = refs.audioContext.current;
    if (!ctx || ctx.state === 'closed' || !refs.masterGain.current) return;
    refs.masterGain.current.gain.linearRampToValueAtTime(voiceConfig?.volume ?? 1.0, ctx.currentTime + 0.05);
    // Re-runs only when volume itself changes — `refs`/`ctx` are read fresh each time, not tracked as deps (mirrors the baseline's own `[voiceConfig?.volume]`-only effect).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [voiceConfig?.volume]);

  useEffect(() => {
    if (!enabled || status !== 'playing') return undefined;
    let cancelled = false;
    const tick = (): void => {
      if (cancelled) return;
      const outcome = computeModelTickOutcome(refs);
      if (outcome.kind === 'closed') {
        refs.raf.current = null;
        return;
      }
      if (outcome.kind === 'done') {
        setStatus('done');
        setSpokenRange(null);
        stopModelAudio(refs);
        onFinished('done');
        refs.raf.current = null;
        return;
      }
      if (outcome.kind === 'progress' && outcome.spokenRange) setSpokenRange(outcome.spokenRange);
      refs.raf.current = requestAnimationFrame(tick);
    };
    refs.raf.current = requestAnimationFrame(tick);
    return () => {
      cancelled = true;
      cancelScheduledFrame(refs);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, status]);

  /** A run failed: stop the audio, report why, and reset the player — the old `tts_error` handler's job. */
  const fail = useCallback(
    (err: unknown) => {
      stopModelAudio(refs);
      setStatus('error');
      setSpokenRange(null);
      onFinished('error');
      onError?.(err instanceof VoiceTransportError ? err.code : 'failed');
    },
    [refs, setStatus, setSpokenRange, onFinished, onError],
  );

  const speak = useCallback(
    (text: string) => {
      if (!enabled || !text || !ttsModel || projectId === undefined) return;

      runRef.current?.abort();
      const run = new AbortController();
      runRef.current = run;
      refs.userPaused.current = false;
      stopModelAudio(refs);
      refs.sentenceWaypoints.current = [];
      refs.fullText.current = text;
      refs.charTimeline.current = buildCharTimeline(text, refs.calibratedRate.current);
      refs.pcmQueue.current = [];
      refs.finalTtsDone.current = false;
      refs.totalEnqueuedSamples.current = 0;

      ensureAudioContext(refs, voiceConfig?.volume ?? 1.0, 24000);
      if (refs.schedulerTimer.current !== null) clearInterval(refs.schedulerTimer.current);
      refs.schedulerTimer.current = setInterval(() => scheduleFromQueue(refs), 25);

      const request = speechRequestFor(ttsModel, projectId, voiceConfig);
      void streamSpeech({ refs, text, request, signal: run.signal }).catch((err: unknown) => {
        if (run.signal.aborted || isVoiceAbort(err) || runRef.current !== run) return;
        fail(err);
      });

      setStatus('playing');
      setSpokenRange(null);
    },
    [enabled, ttsModel, projectId, voiceConfig, setStatus, setSpokenRange, refs, fail],
  );

  const pause = useCallback(() => {
    if (!enabled || status !== 'playing') return;
    refs.userPaused.current = true;
    void refs.audioContext.current?.suspend();
    setStatus('paused');
  }, [enabled, status, setStatus, refs]);

  const resume = useCallback(() => {
    if (!enabled || status !== 'paused') return;
    refs.userPaused.current = false;
    void refs.audioContext.current?.resume();
    setStatus('playing');
  }, [enabled, status, setStatus, refs]);

  const stop = useCallback(() => {
    if (!enabled) return;
    runRef.current?.abort();
    runRef.current = null;
    refs.userPaused.current = false;
    stopModelAudio(refs);
    refs.fullText.current = '';
    setStatus('idle');
    setSpokenRange(null);
    onFinished('idle');
  }, [enabled, setStatus, setSpokenRange, refs, onFinished]);

  // Cleanup on unmount (or `enabled` flipping off mid-playback).
  useEffect(() => {
    return () => {
      if (!enabled) return;
      runRef.current?.abort();
      stopModelAudio(refs);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled]);

  return { speak, pause, resume, stop };
}
