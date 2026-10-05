import { useState } from 'react';

import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { voiceTransportMock } from '../__mocks__/voiceTransport.mock';

import { VoiceTransportError } from '@/shared/api/voiceTransport';
import type { SpeechRequest } from '@/shared/api/voiceTransport';

import { useModelTtsEngine, type UseModelTtsEngineParams } from './useModelTtsEngine.hooks';
import type { TtsModel, TtsSpokenRange, TtsStatus } from './useTextToSpeech.types';

/* ── the HTTPS speech route — each call answers when the test says so ── */

interface PendingSpeech {
  readonly request: SpeechRequest;
  readonly signal: AbortSignal | undefined;
  readonly resolve: (audio: ArrayBuffer) => void;
  readonly reject: (err: unknown) => void;
}

const speechCalls: PendingSpeech[] = [];
const synthesizeSpeech = voiceTransportMock.synthesizeSpeech;

/* ── FakeAudioContext — controllable currentTime, no real audio hardware ── */

class FakeGainNode {
  gain = { value: 1, setValueAtTime: vi.fn(), linearRampToValueAtTime: vi.fn() };
  connect = vi.fn();
}

class FakeBufferSource {
  buffer: unknown;
  onended: (() => void) | null = null;
  connect = vi.fn();
  start = vi.fn();
  stop = vi.fn();
}

class FakeAudioBuffer {
  readonly duration: number;
  constructor(
    _channels: number,
    length: number,
    sampleRate: number,
  ) {
    this.duration = length / sampleRate;
  }
  copyToChannel = vi.fn();
}

/** What `decodeAudioData` answers: `byteLength` samples of a constant tone, at the context's rate. */
function decodedBuffer(byteLength: number): AudioBuffer {
  const samples = new Float32Array(byteLength).fill(0.5);
  return { length: samples.length, sampleRate: 24000, getChannelData: () => samples } as unknown as AudioBuffer;
}

let lastCreatedContext: FakeAudioContext | undefined;

class FakeAudioContext {
  state: 'running' | 'suspended' | 'closed' = 'running';
  currentTime = 0;
  outputLatency = 0;
  destination = {};
  createGain = (): FakeGainNode => new FakeGainNode();
  createBuffer = (channels: number, length: number, sampleRate: number): FakeAudioBuffer => new FakeAudioBuffer(channels, length, sampleRate);
  createBufferSource = (): FakeBufferSource => new FakeBufferSource();
  decodeAudioData = vi.fn((audio: ArrayBuffer) => Promise.resolve(decodedBuffer(audio.byteLength)));
  resume = vi.fn(() => {
    this.state = 'running';
    return Promise.resolve();
  });
  suspend = vi.fn(() => {
    this.state = 'suspended';
    return Promise.resolve();
  });
  close = vi.fn(() => {
    this.state = 'closed';
    return Promise.resolve();
  });
  constructor() {
    // eslint-disable-next-line typescript/no-this-alias -- test double: records the instance under construction so assertions can reach it, not a pre-ES2015 scope workaround.
    lastCreatedContext = this;
  }
}

/* ── requestAnimationFrame — a manually-driven queue instead of real frame timing ── */

let rafQueue: Map<number, FrameRequestCallback>;
let rafNextId: number;

function stubRaf(): void {
  rafQueue = new Map();
  rafNextId = 1;
  // `vi.stubGlobal` (not a bare `window.foo =` assignment): the hook under
  // test calls the UNQUALIFIED `requestAnimationFrame`/`cancelAnimationFrame`
  // identifiers, which resolve through Node's global scope in vitest's jsdom
  // project rather than always aliasing `window`'s own property.
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback): number => {
    const id = rafNextId++;
    rafQueue.set(id, cb);
    return id;
  });
  vi.stubGlobal('cancelAnimationFrame', (id: number): void => {
    rafQueue.delete(id);
  });
}

/** Runs every currently-queued rAF callback once (callbacks that themselves queue a new frame appear in the NEXT flush, not this one). */
function flushRaf(): void {
  const due = [...rafQueue.entries()];
  rafQueue.clear();
  for (const [, cb] of due) cb(0);
}

const TTS_MODEL: TtsModel = { id: 'p1_voice-model', name: 'voice-model', project_id: 'p1', default: true };

interface Harness {
  readonly speak: (text: string) => void;
  readonly pause: () => void;
  readonly resume: () => void;
  readonly stop: () => void;
  readonly status: TtsStatus;
  readonly spokenRange: TtsSpokenRange | null;
}

type HarnessParams = Omit<UseModelTtsEngineParams, 'status' | 'setStatus' | 'setSpokenRange' | 'onFinished' | 'projectId'> & {
  readonly projectId?: string | number | undefined;
  readonly onFinished?: (s: 'done' | 'error' | 'idle') => void;
};

function useHarness(params: HarnessParams): Harness {
  const [status, setStatus] = useState<TtsStatus>('idle');
  const [spokenRange, setSpokenRange] = useState<TtsSpokenRange | null>(null);
  const engine = useModelTtsEngine({
    ...params,
    projectId: 'projectId' in params ? params.projectId : '7',
    status,
    setStatus,
    setSpokenRange,
    onFinished: params.onFinished ?? (() => {}),
  });
  return { ...engine, status, spokenRange };
}

/** Lets the speech/decoding promise chain run. */
async function settle(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
}

describe('useModelTtsEngine', () => {
  let originalAudioContext: typeof window.AudioContext | undefined;

  beforeEach(() => {
    vi.useFakeTimers();
    originalAudioContext = window.AudioContext;
    // @ts-expect-error -- test double, not the real DOM constructor shape.
    window.AudioContext = FakeAudioContext;
    stubRaf();
    lastCreatedContext = undefined;
    speechCalls.length = 0;
    synthesizeSpeech.mockReset();
    synthesizeSpeech.mockImplementation(
      (request: SpeechRequest, signal?: AbortSignal) =>
        new Promise<ArrayBuffer>((resolve, reject) => {
          speechCalls.push({ request, signal, resolve, reject });
        }),
    );
  });

  afterEach(() => {
    window.AudioContext = originalAudioContext as typeof window.AudioContext;
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('when disabled, speak/pause/resume/stop are no-ops', () => {
    const { result } = renderHook(() => useHarness({ enabled: false, ttsModel: TTS_MODEL, voiceConfig: {} }));
    act(() => result.current.speak('hello'));
    expect(synthesizeSpeech).not.toHaveBeenCalled();
    expect(result.current.status).toBe('idle');
  });

  it('speak() requests the first sentence with the project, model, voice and speed, and sets status to playing', () => {
    const { result } = renderHook(() =>
      useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: { voiceId: 'v-1', rate: 1.5, volume: 0.8 } }),
    );
    act(() => result.current.speak('Hello world. Bye.'));

    // One sentence at a time: the second is requested once the first answers.
    expect(speechCalls).toHaveLength(1);
    expect(speechCalls[0]?.request).toEqual({
      projectId: '7',
      model: 'voice-model',
      input: 'Hello world.',
      voice: 'v-1',
      speed: 1.5,
      instructions: undefined,
    });
    expect(result.current.status).toBe('playing');
  });

  it("defaults the voice to 'alloy' and pins the gpt-4o TTS persona instructions", () => {
    const model: TtsModel = { id: 'p1_gpt', name: 'gpt-4o-mini-tts', project_id: 'p1' };
    const { result } = renderHook(() => useHarness({ enabled: true, ttsModel: model, voiceConfig: {} }));
    act(() => result.current.speak('Hi'));

    expect(speechCalls[0]?.request.voice).toBe('alloy');
    expect(speechCalls[0]?.request.instructions).toContain('calm and warm');
  });

  it('an empty text, a missing ttsModel, or a missing project sends no request and keeps status idle', () => {
    const { result: noText } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: {} }));
    act(() => noText.current.speak(''));

    const { result: noModel } = renderHook(() => useHarness({ enabled: true, ttsModel: null, voiceConfig: {} }));
    act(() => noModel.current.speak('hi'));

    const { result: noProject } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, projectId: undefined, voiceConfig: {} }));
    act(() => noProject.current.speak('hi'));

    expect(synthesizeSpeech).not.toHaveBeenCalled();
    expect(noProject.current.status).toBe('idle');
  });

  it('plays every sentence in order: request -> decode -> queue -> scheduler -> RAF to the done status', async () => {
    const onFinished = vi.fn();
    const { result } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: {}, onFinished }));

    act(() => result.current.speak('One. Two.'));
    speechCalls[0]?.resolve(new ArrayBuffer(240));
    await settle();
    // The first answer prefetched the second sentence.
    expect(speechCalls).toHaveLength(2);
    expect(speechCalls[1]?.request.input).toBe('Two.');
    speechCalls[1]?.resolve(new ArrayBuffer(240));
    await settle();
    expect(lastCreatedContext?.decodeAudioData).toHaveBeenCalledTimes(2);

    // Scheduler tick: the final sentence is queued, so the pre-roll wait is skipped.
    act(() => {
      vi.advanceTimersByTime(25);
    });
    act(() => flushRaf());
    expect(result.current.status).toBe('playing');

    const ctx = lastCreatedContext;
    if (ctx) ctx.currentTime = 999;
    act(() => flushRaf());

    expect(result.current.status).toBe('done');
    expect(result.current.spokenRange).toBeNull();
    expect(onFinished).toHaveBeenCalledWith('done');
    expect(ctx?.close).toHaveBeenCalled();
  });

  it('pause() suspends the AudioContext and sets status to paused; resume() resumes it and sets status back to playing', () => {
    const { result } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: {} }));
    act(() => result.current.speak('Hi'));

    act(() => result.current.pause());
    expect(result.current.status).toBe('paused');
    expect(lastCreatedContext?.suspend).toHaveBeenCalled();

    act(() => result.current.resume());
    expect(result.current.status).toBe('playing');
    expect(lastCreatedContext?.resume).toHaveBeenCalled();
  });

  it('pause() while not playing, and resume() while not paused, are no-ops', () => {
    const { result } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: {} }));
    act(() => result.current.pause());
    expect(result.current.status).toBe('idle');
    act(() => result.current.resume());
    expect(result.current.status).toBe('idle');
  });

  it('stop() aborts the request in flight, closes the AudioContext, resets status to idle, and calls onFinished("idle")', async () => {
    const onFinished = vi.fn();
    const onError = vi.fn();
    const { result } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: {}, onFinished, onError }));
    act(() => result.current.speak('Hi'));
    const ctx = lastCreatedContext;

    act(() => result.current.stop());

    expect(speechCalls[0]?.signal?.aborted).toBe(true);
    expect(ctx?.close).toHaveBeenCalled();
    expect(result.current.status).toBe('idle');
    expect(result.current.spokenRange).toBeNull();
    expect(onFinished).toHaveBeenCalledWith('idle');

    // A late answer for the stopped run is neither decoded nor reported.
    speechCalls[0]?.resolve(new ArrayBuffer(240));
    await settle();
    expect(ctx?.decodeAudioData).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });

  it('a refused speech request stops playback, sets status to error, and reports why', async () => {
    const onFinished = vi.fn();
    const onError = vi.fn();
    const { result } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: {}, onFinished, onError }));
    act(() => result.current.speak('Hi'));

    speechCalls[0]?.reject(new VoiceTransportError('model-unavailable'));
    await settle();

    expect(result.current.status).toBe('error');
    expect(result.current.spokenRange).toBeNull();
    expect(onFinished).toHaveBeenCalledWith('error');
    expect(onError).toHaveBeenCalledWith('model-unavailable');
  });

  it("an unexpected failure is reported as 'failed'", async () => {
    const onError = vi.fn();
    const { result } = renderHook(() => useHarness({ enabled: true, ttsModel: TTS_MODEL, voiceConfig: {}, onError }));
    act(() => result.current.speak('Hi'));

    speechCalls[0]?.reject(new Error('decoder crashed'));
    await settle();

    expect(onError).toHaveBeenCalledWith('failed');
  });
});
