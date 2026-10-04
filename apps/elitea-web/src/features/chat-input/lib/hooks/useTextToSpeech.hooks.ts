/**
 * Ported from
 * apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js
 * (871 lines) — the public `speak`/`stop`/`pause`/`resume` TTS API, backed
 * by either a server-side model (Socket.IO + Web Audio,
 * `useModelTtsEngine.hooks.ts`) or the browser `SpeechSynthesis` fallback
 * (`useBrowserTtsEngine.hooks.ts`), picked by `hasModelTTS =
 * !!(ttsModel && socket && isAudioContextSupported())` — same condition as
 * the baseline. This file OWNS `status`/`spokenRange`/`showPlayer`/
 * `speakableText` (shared, reactive state neither engine keeps privately —
 * see both engines' own module docs) and is otherwise a thin dispatcher
 * over whichever engine is currently active.
 *
 * `socket` is accepted as an explicit parameter (matching the baseline's
 * own signature, sourced there from `useContext(SocketContext)` at each
 * call site) rather than this hook calling `useSocketClient()` itself: as
 * of this unit, no `app/` file mounts a `SocketClientContext.Provider` yet
 * (`shared/api/socket/client.ts`'s own doc comment) AND `hasModelTTS`'s `socket` operand is a legitimately
 * optional, "gracefully degrade to browser TTS" input in the baseline
 * itself, not a programmer error `useSocketClient()`'s throw-if-absent
 * contract would be correct for. Callers read the client via
 * `useContext(SocketClientContext)` (degrading to `null`) and pass it down.
 */
import { useCallback, useState } from 'react';

import type { SocketClient } from '@/shared/api/socket/client';

import { useBrowserTtsEngine } from './useBrowserTtsEngine.hooks';
import { useModelTtsEngine } from './useModelTtsEngine.hooks';
import type { TtsModel, TtsSpokenRange, TtsStatus, TtsVoiceConfig } from './useTextToSpeech.types';

function isAudioContextSupported(): boolean {
  return typeof window !== 'undefined' && 'AudioContext' in window;
}

/** @public */
export interface UseTextToSpeechParams {
  readonly ttsModel?: TtsModel | null | undefined;
  readonly socket?: SocketClient | null | undefined;
  readonly voiceConfig?: TtsVoiceConfig | undefined;
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
 * TTS playback hook using either a server-side TTS model (Socket.IO + Web
 * Audio) when `ttsModel` + `socket` are both provided (and `AudioContext`
 * exists), or the browser `SpeechSynthesis` API as a fallback.
 */
export function useTextToSpeech(params: UseTextToSpeechParams = {}): UseTextToSpeechResult {
  const { ttsModel, socket, voiceConfig } = params;
  const [showPlayer, setShowPlayer] = useState(false);
  const [speakableText, setSpeakableText] = useState('');
  const [status, setStatus] = useState<TtsStatus>('idle');
  const [spokenRange, setSpokenRange] = useState<TtsSpokenRange | null>(null);

  const hasModelTTS = !!(ttsModel && socket && isAudioContextSupported());

  /** `useTextToSpeech.hooks.js`'s `resetStatus`'s UI-reset half — the status transition itself is already applied by whichever engine calls this (its own `setStatus`), matching the baseline's combined `resetStatus(newStatus)` behaviour when split across the two engines. */
  const resetPlayerUi = useCallback(() => {
    setShowPlayer(false);
    setSpeakableText('');
  }, []);

  const modelEngine = useModelTtsEngine({
    enabled: hasModelTTS,
    status,
    ttsModel,
    socket,
    voiceConfig,
    setStatus,
    setSpokenRange,
    onFinished: resetPlayerUi,
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

  const speak = useCallback(
    (text: string) => {
      if (!text) return;
      active.speak(text);
    },
    [active],
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

  const isSupported = hasModelTTS || (typeof window !== 'undefined' && 'speechSynthesis' in window);

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
