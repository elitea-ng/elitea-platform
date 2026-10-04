/**
 * Ported from
 * apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js
 * (871 lines) — the public `speak`/`stop`/`pause`/`resume` TTS API, backed
 * by either a server-side model (HTTPS + Web Audio,
 * `useModelTtsEngine.hooks.ts`) or the browser `SpeechSynthesis` fallback
 * (`useBrowserTtsEngine.hooks.ts`), picked by `hasModelTTS =
 * !!(ttsModel && projectId && isAudioContextSupported())`. This file OWNS
 * `status`/`spokenRange`/`showPlayer`/`speakableText` (shared, reactive state
 * neither engine keeps privately — see both engines' own module docs) and is
 * otherwise a thin dispatcher over whichever engine is currently active.
 *
 * The baseline's condition was `ttsModel && socket && …`. The socket is gone
 * (`api/voiceTransport.ts`); the model engine needs the project instead,
 * because the `/llm` edge bills the project the request names.
 *
 * `onError` is how a failure becomes a message instead of silence: a failed
 * speech request, and a `speak()` with nothing able to speak (no model and no
 * browser `speechSynthesis`) — `'no-model'`.
 */
import { useCallback, useState } from 'react';

import type { VoiceProblem } from '../voiceProblems';

import { useBrowserTtsEngine } from './useBrowserTtsEngine.hooks';
import { useModelTtsEngine } from './useModelTtsEngine.hooks';
import type { TtsModel, TtsSpokenRange, TtsStatus, TtsVoiceConfig } from './useTextToSpeech.types';

function isAudioContextSupported(): boolean {
  return typeof window !== 'undefined' && 'AudioContext' in window;
}

/** @public */
export interface UseTextToSpeechParams {
  readonly ttsModel?: TtsModel | null | undefined;
  /** The project the user works in. The model engine needs it; without it the browser engine speaks. */
  readonly projectId?: string | number | undefined;
  readonly voiceConfig?: TtsVoiceConfig | undefined;
  readonly onError?: ((problem: VoiceProblem) => void) | undefined;
}

/** @public */
export interface UseTextToSpeechResult {
  readonly speak: (text: string) => void;
  readonly stop: () => void;
  readonly pause: () => void;
  readonly resume: () => void;
  readonly isPlaying: boolean;
  readonly isPaused: boolean;
  /** `hasModelTTS || speechSynthesis is available` — false means nothing at all can speak. */
  readonly isSupported: boolean;
  readonly spokenRange: TtsSpokenRange | null;
  readonly showPlayer: boolean;
  readonly setShowPlayer: (show: boolean) => void;
  readonly speakableText: string;
  readonly setSpeakableText: (text: string) => void;
}

/**
 * TTS playback hook using either a server-side TTS model (HTTPS + Web Audio)
 * when `ttsModel` + `projectId` are both provided (and `AudioContext`
 * exists), or the browser `SpeechSynthesis` API as a fallback.
 */
export function useTextToSpeech(params: UseTextToSpeechParams = {}): UseTextToSpeechResult {
  const { ttsModel, projectId, voiceConfig, onError } = params;
  const [showPlayer, setShowPlayer] = useState(false);
  const [speakableText, setSpeakableText] = useState('');
  const [status, setStatus] = useState<TtsStatus>('idle');
  const [spokenRange, setSpokenRange] = useState<TtsSpokenRange | null>(null);

  const hasModelTTS = !!(ttsModel && projectId !== undefined && isAudioContextSupported());

  /** `useTextToSpeech.hooks.js`'s `resetStatus`'s UI-reset half — the status transition itself is already applied by whichever engine calls this (its own `setStatus`), matching the baseline's combined `resetStatus(newStatus)` behaviour when split across the two engines. */
  const resetPlayerUi = useCallback(() => {
    setShowPlayer(false);
    setSpeakableText('');
  }, []);

  const modelEngine = useModelTtsEngine({
    enabled: hasModelTTS,
    status,
    ttsModel,
    projectId,
    voiceConfig,
    setStatus,
    setSpokenRange,
    onFinished: resetPlayerUi,
    onError,
  });

  const browserEngine = useBrowserTtsEngine({
    enabled: !hasModelTTS,
    status,
    voiceConfig,
    setStatus,
    setSpokenRange,
    onFinished: resetPlayerUi,
  });

  const active = hasModelTTS ? modelEngine : browserEngine;
  const isSupported = hasModelTTS || (typeof window !== 'undefined' && 'speechSynthesis' in window);

  const speak = useCallback(
    (text: string) => {
      if (!text) return;
      // Nothing can speak: say so, rather than arm a player that stays silent.
      if (!isSupported) {
        onError?.('no-model');
        return;
      }
      active.speak(text);
    },
    [active, isSupported, onError],
  );

  const stop = useCallback(() => {
    active.stop();
  }, [active]);

  const pause = useCallback(() => {
    active.pause();
  }, [active]);

  const resume = useCallback(() => {
    active.resume();
  }, [active]);

  return {
    speak,
    stop,
    pause,
    resume,
    isPlaying: status === 'playing',
    isPaused: status === 'paused',
    isSupported,
    spokenRange,
    showPlayer,
    setShowPlayer,
    speakableText,
    setSpeakableText,
  };
}
