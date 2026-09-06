/**
 * Voice-personalization helpers — the data access and the pure functions
 * behind `VoicePersonalizationSection`.
 *
 * Split out of that component only because the file passed the 400-line
 * budget once the sliders were rebuilt against MUI's own `Slider`; nothing
 * else imports these.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { createStorage } from '@/shared/lib/storage';

export interface VoiceConfig {
  voiceName: string | null;
  voiceId: string | null;
  rate: number;
  volume: number;
}

/** One server-side TTS voice — mirrors `features/chat-input/api/ttsVoices.ts`'s `TtsVoice`. */
export interface SettingsTtsVoice {
  readonly id?: string;
  readonly name: string;
}

/**
 * `GET /configurations/tts_voices/{projectId}?model_name=...` — local,
 * disclosed near-duplicate of `features/chat-input/api/ttsVoices.ts`'s
 * `getTtsVoices` (same route/shape that file's own doc comment already
 * discloses duplicating from `features/credentials`). `no-sideways-features`
 * (`.dependency-cruiser.cjs`) forbids `features/settings` importing either
 * feature-private module, so this is copied rather than reached into.
 */
export async function fetchSettingsTtsVoices(
  projectId: string,
  modelName: string | undefined,
): Promise<readonly SettingsTtsVoice[]> {
  const search = new URLSearchParams();
  if (modelName !== undefined) search.append('model_name', modelName);
  const envelope = await eliteaFetch<{ data?: { voices?: readonly SettingsTtsVoice[] } }>(
    `/configurations/tts_voices/${projectId}?${search.toString()}`,
  );
  return envelope.data?.voices ?? [];
}

/** One TTS model row — only the fields this panel reads. */
export interface SettingsTtsModel {
  readonly name: string;
  readonly project_id?: string;
  readonly default?: boolean;
}

/**
 * `GET /configurations/models/{projectId}?section=tts&include_shared=true`
 * — the TTS SECTION of the model catalogue.
 *
 * The panel used to call `shared/api/configurationsApi`'s
 * `useListModelsQuery`, which sends no `section` at all. That list holds
 * every model type, so the first / default row is normally an LLM, and the
 * panel treated that LLM as its TTS model. `section=tts` is what the
 * sibling port `features/chat-input/api/models.ts` already sends.
 *
 * Local, disclosed near-duplicate of that sibling fetcher, for the same
 * reason `fetchSettingsTtsVoices` above is one: `no-sideways-features`
 * (`.dependency-cruiser.cjs`) forbids `features/settings` importing a
 * `features/chat-input` internal.
 */
export async function fetchSettingsTtsModels(projectId: string): Promise<readonly SettingsTtsModel[]> {
  const search = new URLSearchParams({ section: 'tts', include_shared: 'true' });
  const envelope = await eliteaFetch<{ data?: { items?: readonly SettingsTtsModel[] } }>(
    `/configurations/models/${projectId}?${search.toString()}`,
  );
  return envelope.data?.items ?? [];
}

/**
 * Whether the panel should offer the MODEL voices rather than the browser
 * ones. A pure function, both to keep the component under the §3.5
 * complexity budget and because this is the rule the defect broke: the
 * fallback used to be gated on `hasModelTTS` alone.
 */
export function shouldUseModelVoices(hasModelTTS: boolean, settled: boolean, voiceCount: number): boolean {
  return hasModelTTS && (!settled || voiceCount > 0);
}

/** The currently-selected voice, read from whichever field the active mode writes; extracted for the same complexity reason. */
export function pickSelectedVoice(config: VoiceConfig, useModelVoices: boolean): string {
  if (useModelVoices) return config.voiceId ?? '';
  return config.voiceName ?? '';
}

/** Plays the browser-voice preview; extracted for the same complexity reason. */
export function speakBrowserPreview(
  rate: number | undefined,
  volume: number | undefined,
  setIsPlaying: (playing: boolean) => void,
): void {
  if (typeof window === 'undefined' || !('speechSynthesis' in window)) return;
  const utterance = new SpeechSynthesisUtterance(VOICE_PREVIEW_TEXT);
  utterance.rate = rate ?? 1.0;
  utterance.volume = volume ?? 1.0;
  setIsPlaying(true);
  utterance.onend = () => setIsPlaying(false);
  utterance.onerror = () => setIsPlaying(false);
  window.speechSynthesis.speak(utterance);
}

/** Maps either voice shape to `SingleSelect` options; extracted for the same complexity reason. */
export function toVoiceOptions(
  voices: readonly (SettingsTtsVoice | { name: string; localService: boolean })[],
  useModelVoices: boolean,
): Array<{ value: string; label: string }> {
  return voices
    .map((v) => {
      if (useModelVoices) {
        const voice = v as SettingsTtsVoice;
        return { value: voice.id ?? voice.name, label: voice.name };
      }
      const voice = v as { name: string; localService: boolean };
      return { value: voice.name, label: `${voice.name}${voice.localService ? '' : ' (online)'}` };
    })
    .filter((v) => v.value !== '');
}

/** `1.5` -> `1.5×` in the drag bubble — baseline `valueLabelFormat`. */
export function formatSpeedLabel(value: number): string {
  return `${value}\u00d7`;
}

/** `0.5` -> `50%` in the drag bubble — baseline `valueLabelFormat`. */
export function formatPercentLabel(value: number): string {
  return `${Math.round(value * 100)}%`;
}

const STORAGE_KEY = 'chat-input.voice-config';

/** Persists the config under the key the rest of the app reads it from. */
export function storeVoiceConfig(config: VoiceConfig): void {
  try {
    createStorage('local').set(STORAGE_KEY, JSON.stringify(config));
  } catch {
    // Ignore — a private window with storage blocked must not break the panel.
  }
}
const DEFAULT_CONFIG: VoiceConfig = { voiceName: null, voiceId: null, rate: 1.0, volume: 1.0 };

const VOICE_PREVIEW_TEXT = 'Hello, this is a voice preview.';
/*
 * FOUR marks, and the labels carry a plain "x" — `[fsd]/features/chat/
 * voice-config/constants/voice.constants.js`. This list used to be three
 * entries with a multiplication sign, fed to `shared/ui`'s `DiscreteSlider`.
 * That component is INTEGER-stepped: it builds its own marks as
 * `max - min + 1` whole steps and ignores fractional level values, so a
 * 0.5→2 scale rendered marks at "0.5" and "1.5" only, and the 0→1 volume
 * scale rendered "0" and "1" instead of 0%/50%/100%. Both sliders now use
 * MUI's `Slider` directly with an explicit `marks` array and a fractional
 * `step`, which is what the baseline does.
 */
export const VOICE_SPEED_MARKS = [
  { value: 0.5, label: '0.5x' },
  { value: 1.0, label: '1x' },
  { value: 1.5, label: '1.5x' },
  { value: 2.0, label: '2x' },
];
export const VOICE_VOLUME_MARKS = [
  { value: 0, label: '0%' },
  { value: 0.5, label: '50%' },
  { value: 1, label: '100%' },
];

export function loadStored(): VoiceConfig {
  const store = createStorage('local');
  const raw = store.get(STORAGE_KEY);
  if (raw === null) return { ...DEFAULT_CONFIG };
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (typeof parsed === 'object' && parsed !== null) {
      const p = parsed as Record<string, unknown>;
      return {
        voiceName: (p.voiceName as string | null) ?? null,
        voiceId: (p.voiceId as string | null) ?? null,
        rate: typeof p.rate === 'number' ? Math.max(0.5, Math.min(2, p.rate)) : 1.0,
        volume: typeof p.volume === 'number' ? Math.max(0, Math.min(1, p.volume)) : 1.0,
      };
    }
  } catch {
    // Ignore.
  }
  return { ...DEFAULT_CONFIG };
}

