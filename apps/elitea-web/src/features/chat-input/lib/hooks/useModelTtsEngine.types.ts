/**
 * Refs bag shared by `useModelTtsEngine.hooks.ts`'s three sibling
 * implementation files (`.scheduler.ts`/`.stream.ts`/`.raf.ts`) — the model
 * (server-side, Web Audio) TTS engine's mutable playback state, ported from
 * `useTextToSpeech.hooks.js`'s ~19 independent `useRef`s (lines 108-171).
 * Bundled into one object so the scheduling/stream/highlight logic can be
 * split across small, independently-testable functions without each one
 * re-declaring the same long parameter list.
 */
import type { RefObject } from 'react';

import type { TtsWaypoint } from '../helpers/ttsHighlight.helpers';
import type { CharTimeline } from '../helpers/ttsTimeline.helpers';

export interface PendingPcmChunk {
  readonly samples: Float32Array<ArrayBuffer>;
  readonly sampleRate: number;
}

export interface ModelTtsRefs {
  readonly audioContext: RefObject<AudioContext | null>;
  readonly masterGain: RefObject<GainNode | null>;
  readonly nextStartTime: RefObject<number>;
  readonly scheduledSources: RefObject<AudioBufferSourceNode[]>;
  /** `null` = playback has not started yet. */
  readonly playStartTime: RefObject<number | null>;
  readonly totalDuration: RefObject<number>;
  readonly allChunksReceived: RefObject<boolean>;
  /** True only when the user explicitly paused — distinguishes user-pause from browser auto-suspend. */
  readonly userPaused: RefObject<boolean>;
  /** Self-calibrating chars/second rate, intentionally NOT reset between sessions (accumulates across the hook's lifetime). */
  readonly calibratedRate: RefObject<number>;
  readonly charTimeline: RefObject<CharTimeline | null>;
  readonly sentenceWaypoints: RefObject<TtsWaypoint[]>;
  readonly pcmQueue: RefObject<PendingPcmChunk[]>;
  readonly schedulerTimer: RefObject<ReturnType<typeof setInterval> | null>;
  readonly finalTtsDone: RefObject<boolean>;
  readonly totalEnqueuedSamples: RefObject<number>;
  readonly sampleRate: RefObject<number>;
  readonly fullText: RefObject<string>;
  readonly raf: RefObject<number | null>;
}
