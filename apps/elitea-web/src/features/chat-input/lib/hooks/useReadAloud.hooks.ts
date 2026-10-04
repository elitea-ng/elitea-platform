/**
 * Ported from
 * apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useReadAloud.hooks.js —
 * the read-aloud / TTS glue hook shared by every surface that renders a
 * voice mini-player. Picks a TTS model (`section: 'tts'`) for server-side
 * speech, falling back to the browser's `SpeechSynthesis`, and tracks the
 * spoken word range for in-bubble highlighting.
 *
 * `useModelsList`/`useTtsVoices` — this slice's OWN `api/models.ts` (built
 * first by the sibling ASR unit, parameterized by `section` per this unit's
 * shared-fetcher build brief — see that file's own doc comment) and
 * `api/ttsVoices.ts` — replace the baseline's `useListModelsQuery`/
 * `useGetTtsVoicesQuery` RTK Query hooks (spec §2.3: TanStack Query, no
 * RTK Query anywhere in the new app).
 *
 * Model speech goes over HTTPS to `/llm/v1/audio/speech`
 * (`api/voiceTransport.ts`); it used to take a socket.io client that had no
 * server behind it. `onError` receives the readable message for a failed
 * read-aloud (`../voiceProblems.ts`), so the caller can show it.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { useModelsList } from '../../api/models';
import type { TtsVoice } from '../../api/ttsVoices';
import { useTtsVoices } from '../../api/ttsVoices';
import type { SpeakableText, TtsSegment } from '../helpers/ttsHelpers';
import { toSpeakableText } from '../helpers/ttsHelpers';
import { voiceProblemMessage } from '../voiceProblems';
import type { VoiceProblem } from '../voiceProblems';

import { useTextToSpeech } from './useTextToSpeech.hooks';
import type { TtsModel, TtsSpokenRange } from './useTextToSpeech.types';
import type { VoiceConfig, VoiceConfigUpdate } from './useVoiceConfig.hooks';
import { useVoiceConfig } from './useVoiceConfig.hooks';

/** @public The props a caller spreads onto its voice mini-player / control button. */
export interface VoicePlayerProps {
  readonly voiceConfig: VoiceConfig;
  /** `hasModelTTS`: server voice rows (`TtsVoice`); otherwise the browser's own `SpeechSynthesisVoice` list. */
  readonly voices: readonly (TtsVoice | SpeechSynthesisVoice)[];
  readonly onVoiceConfigChange: (updates: VoiceConfigUpdate) => void;
  readonly ttsModel: TtsModel | null;
  readonly hasModelTTS: boolean;
  /** The project the voice settings preview bills. */
  readonly projectId: string | undefined;
  readonly isPlaying: boolean;
  readonly onStop: () => void;
  readonly onPlay: () => void;
}

/** @public */
export interface UseReadAloudParams {
  readonly projectId: string | undefined;
  /** Receives the readable message when read-aloud fails or nothing can speak. */
  readonly onError?: ((message: string) => void) | undefined;
}

/** @public */
export interface UseReadAloudResult {
  readonly onAutoSpeak: (text: string, msgId?: string | number | null) => void;
  readonly speakingMessageId: string | number | null;
  readonly speakingSegments: readonly TtsSegment[] | null;
  readonly spokenRange: TtsSpokenRange | null;
  readonly showPlayer: boolean;
  readonly isPlaying: boolean;
  readonly stop: () => void;
  readonly voicePlayerProps: VoicePlayerProps;
}

function pickDefaultModel(items: readonly TtsModel[] | undefined): TtsModel | null {
  if (!items || items.length === 0) return null;
  return items.find((model) => model.default) ?? items[0] ?? null;
}

function defaultVoiceId(voices: readonly TtsVoice[] | undefined): string | undefined {
  const first = voices?.[0];
  return first ? (first.id ?? first.name) : undefined;
}

export function useReadAloud(params: UseReadAloudParams): UseReadAloudResult {
  const { projectId, onError } = params;
  const [speakingMessageId, setSpeakingMessageId] = useState<string | number | null>(null);
  const [speakingSegments, setSpeakingSegments] = useState<readonly TtsSegment[] | null>(null);

  const { data: ttsModelsData } = useModelsList({ projectId, section: 'tts', includeShared: true }, { enabled: !!projectId });
  const ttsModel = useMemo(() => pickDefaultModel(ttsModelsData?.items), [ttsModelsData]);
  const hasModelTTS = !!(ttsModel && projectId);
  const handleProblem = useCallback((problem: VoiceProblem) => onError?.(voiceProblemMessage(problem)), [onError]);

  // `persist: true` (A9, ELITEA-1312/1313/1315): `voicePlayerProps.
  // onVoiceConfigChange` is the ONLY way a caller can change this hook's
  // `voiceConfig` — it is exposed for exactly one purpose, spreading onto
  // `VoiceControlButton`'s gear-icon `VoiceConfigDialog` (Apply/Cancel).
  // That dialog stages edits in its own local state and only calls this
  // setter on Apply, so an explicit Apply is the only trigger that reaches
  // storage — same `chat-input.voice-config` key `VoicePersonalizationSection`
  // (Settings > Personalization) reads/writes, so a change from either
  // surface is visible on the other.
  const { config: voiceConfig, setConfig: setVoiceConfig, browserVoices, resolvedBrowserVoice } = useVoiceConfig({ persist: true });
  const ttsVoicesQuery = useTtsVoices(
    { projectId: ttsModel?.project_id ?? projectId, modelName: ttsModel?.name },
    { enabled: !!ttsModel },
  );
  /**
   * The voice list the picker offers.
   *
   * In model mode this was permanently EMPTY: `GET /configurations/tts_voices`
   * answered 501 for every project (issue 323), so a user with a TTS model
   * configured saw no voice control at all — `VoiceConfigControls` renders no
   * picker when its option list is empty — while a user without one saw the
   * browser's. The route serves the provider's real catalogue now.
   *
   * An empty answer STILL leaves the picker hidden, and that is deliberate
   * rather than a leftover. It means the provider publishes no catalogue this
   * platform knows, so the model speaks with its own default voice; offering
   * the BROWSER's voices instead would let a user choose one that this mode
   * writes to `voiceId` and sends to a model that has never heard of it. The
   * settings panel makes the opposite choice because its picker also drives a
   * browser-synthesised preview; this one drives the model.
   */
  const displayVoices: readonly (TtsVoice | SpeechSynthesisVoice)[] = hasModelTTS
    ? (ttsVoicesQuery.data?.voices ?? [])
    : browserVoices;

  const {
    speak,
    stop: stopTTS,
    isPlaying,
    isSupported: canSpeak,
    spokenRange,
    showPlayer,
    setShowPlayer,
    speakableText,
    setSpeakableText,
  } = useTextToSpeech({
    ttsModel,
    projectId,
    onError: handleProblem,
    voiceConfig: {
      voice: resolvedBrowserVoice,
      // No pick: the provider's first listed voice is its default (`tts_voices` keeps the provider's order).
      voiceId: voiceConfig.voiceId || defaultVoiceId(ttsVoicesQuery.data?.voices),
      rate: voiceConfig.rate,
      volume: voiceConfig.volume,
    },
  });

  /**
   * Read one answer aloud: arm the player AND start speaking (issue 974).
   *
   * It used to arm only — `setSpeakableText` + `setShowPlayer(true)` — leaving
   * playback to a separate press on the player's play control. That is what
   * "Read out" DOES on this product: the control sits on the answer and says
   * it will read it out, and a person who presses it and hears nothing has no
   * way to know a second, unrelated-looking control in the composer is the one
   * that speaks. The speaking-mode auto-read has the same requirement and no
   * control at all to press.
   *
   * The player still appears, because stopping needs a control and the spoken
   * word range needs somewhere to live.
   */
  const onAutoSpeak = useCallback(
    (text: string, msgId?: string | number | null) => {
      if (!text) return;
      const { text: convertedText, segments }: SpeakableText = toSpeakableText(text);
      if (!convertedText) return;
      // Nothing can speak (no speech model, no browser voice): say so and do
      // not arm a player that would sit there silent.
      if (!canSpeak) {
        handleProblem('no-model');
        return;
      }
      setSpeakingMessageId(msgId ?? null);
      setSpeakingSegments(segments);
      setSpeakableText(convertedText);
      setShowPlayer(true);
      speak(convertedText);
    },
    [canSpeak, handleProblem, setShowPlayer, setSpeakableText, speak],
  );

  const onPlay = useCallback(() => {
    speak(speakableText);
  }, [speak, speakableText]);

  // When playback ends, hide the player and clear the spoken-word highlight.
  useEffect(() => {
    if (!isPlaying) {
      setSpeakingMessageId(null);
      setSpeakingSegments(null);
      setShowPlayer(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isPlaying]);

  return {
    onAutoSpeak,
    speakingMessageId,
    speakingSegments,
    spokenRange,
    showPlayer,
    isPlaying,
    stop: stopTTS,
    voicePlayerProps: {
      voiceConfig,
      voices: displayVoices,
      onVoiceConfigChange: setVoiceConfig,
      ttsModel,
      hasModelTTS,
      projectId,
      isPlaying,
      onStop: stopTTS,
      onPlay,
    },
  };
}
