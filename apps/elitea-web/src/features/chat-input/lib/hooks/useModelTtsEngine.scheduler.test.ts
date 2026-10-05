/**
 * The highlight waypoints follow the audio the user hears, also when a
 * sentence's speech request returns after the previous sentence ended (an
 * underrun). Each sentence is its own HTTP request, so this is the normal
 * case for a short sentence, not an edge case.
 */
import { describe, expect, it } from 'vitest';

import { computeModelTickOutcome } from './useModelTtsEngine.raf';
import { enqueueSamples, scheduleFromQueue } from './useModelTtsEngine.scheduler';
import type { ModelTtsRefs } from './useModelTtsEngine.types';

const RATE = 1000;

interface FakeContext {
  currentTime: number;
  readonly state: AudioContextState;
  readonly outputLatency: number;
  readonly destination: object;
  readonly createBuffer: (channels: number, length: number, rate: number) => { duration: number; copyToChannel: () => void };
  readonly createBufferSource: () => { buffer: unknown; connect: () => void; start: (at: number) => void; onended: unknown };
}

function fakeContext(): FakeContext {
  return {
    currentTime: 0,
    state: 'running',
    outputLatency: 0,
    destination: {},
    createBuffer: (_channels, length, rate) => ({ duration: length / rate, copyToChannel: () => undefined }),
    createBufferSource: () => ({ buffer: null, connect: () => undefined, start: () => undefined, onended: null }),
  };
}

function ref<T>(current: T): { current: T } {
  return { current };
}

function makeRefs(ctx: FakeContext, text: string): ModelTtsRefs {
  return {
    audioContext: ref<AudioContext | null>(ctx as unknown as AudioContext),
    masterGain: ref(null),
    nextStartTime: ref(0),
    scheduledSources: ref([]),
    playStartTime: ref(null),
    totalDuration: ref(0),
    allChunksReceived: ref(false),
    userPaused: ref(false),
    calibratedRate: ref(15.4),
    charTimeline: ref(null),
    sentenceWaypoints: ref([]),
    pcmQueue: ref([]),
    schedulerTimer: ref(null),
    finalTtsDone: ref(false),
    totalEnqueuedSamples: ref(0),
    sampleRate: ref(RATE),
    fullText: ref(text),
    raf: ref(null),
  };
}

function samples(seconds: number): Float32Array<ArrayBuffer> {
  return new Float32Array(Math.round(seconds * RATE));
}

function spokenStart(refs: ModelTtsRefs): number | undefined {
  const outcome = computeModelTickOutcome(refs);
  return outcome.kind === 'progress' ? outcome.spokenRange?.start : undefined;
}

describe('model TTS waypoints under an underrun', () => {
  const TEXT = 'Sure. Here is the second sentence.';
  const FIRST_END = 'Sure.'.length;
  // At the sentence boundary the highlight shows the next word, "Here". A run
  // ahead of the audio shows a later word.
  const BOUNDARY_WORD = TEXT.indexOf('Here');

  it('records each waypoint from the scheduled start, so a late sentence moves its waypoint by the gap', () => {
    const ctx = fakeContext();
    const refs = makeRefs(ctx, TEXT);
    enqueueSamples(refs, samples(0.4), RATE, FIRST_END);
    scheduleFromQueue(refs);
    expect(refs.sentenceWaypoints.current).toEqual([{ charPos: FIRST_END, audioTime: 0.4 }]);

    // The first sentence plays out at 0.4 s; the second answer lands at 1.0 s.
    ctx.currentTime = 1.0;
    enqueueSamples(refs, samples(2), RATE, TEXT.length);
    scheduleFromQueue(refs);

    expect(refs.sentenceWaypoints.current).toEqual([
      { charPos: FIRST_END, audioTime: 0.4 },
      // The gap: the highlight holds at the end of the first sentence until 1.0 s.
      { charPos: FIRST_END, audioTime: 1.0 },
      { charPos: TEXT.length, audioTime: 3.0 },
    ]);
  });

  it('holds the highlight in the first sentence while the next request is still out', () => {
    const ctx = fakeContext();
    const refs = makeRefs(ctx, TEXT);
    enqueueSamples(refs, samples(0.4), RATE, FIRST_END);
    scheduleFromQueue(refs);

    // 0.8 s: the first sentence ended 0.4 s ago and nothing else is queued.
    ctx.currentTime = 0.8;
    expect(spokenStart(refs)).toBe(BOUNDARY_WORD);
  });

  it('does not start the second sentence highlight until its audio starts', () => {
    const ctx = fakeContext();
    const refs = makeRefs(ctx, TEXT);
    enqueueSamples(refs, samples(0.4), RATE, FIRST_END);
    scheduleFromQueue(refs);
    ctx.currentTime = 1.0;
    enqueueSamples(refs, samples(2), RATE, TEXT.length);
    scheduleFromQueue(refs);

    ctx.currentTime = 0.9;
    expect(spokenStart(refs)).toBe(BOUNDARY_WORD);
    ctx.currentTime = 2.0;
    expect(spokenStart(refs)).toBeGreaterThan(BOUNDARY_WORD);
  });
});
