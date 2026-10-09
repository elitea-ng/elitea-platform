/**
 * speechCapture — the microphone half of server recognition.
 *
 * jsdom implements neither `AudioContext`/`AudioWorkletNode` nor
 * `navigator.mediaDevices.getUserMedia`; the fakes below are the ones the old
 * socket.io hook's test used (hand-rolled, `vi.stubGlobal`-installed).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  AUDIO_CHUNK_PROCESSOR_NAME,
  SPEECH_FRAME_SAMPLES,
  SPEECH_SAMPLE_RATE,
  mapGetUserMediaError,
  resampleLinear,
  startSpeechCapture,
} from './speechCapture';

class FakeGainNode {
  gain = { value: 0 };
  connect = vi.fn();
}
class FakeSourceNode {
  connect = vi.fn();
}
class FakeAudioWorkletNode {
  static instances: FakeAudioWorkletNode[] = [];
  port: { postMessage: ReturnType<typeof vi.fn>; onmessage: ((e: MessageEvent<Float32Array>) => void) | null } = {
    postMessage: vi.fn(),
    onmessage: null,
  };
  connect = vi.fn();
  disconnect = vi.fn();
  name: string;

  constructor(_context: unknown, name: string) {
    this.name = name;
    FakeAudioWorkletNode.instances.push(this);
  }
}

class FakeAudioContext {
  static instances: FakeAudioContext[] = [];
  static failAddModule = false;
  sampleRate = 44100;
  state: 'running' | 'closed' = 'running';
  destination = {};
  audioWorklet = {
    addModule: vi.fn((_moduleUrl: string) => (FakeAudioContext.failAddModule ? Promise.reject(new Error('no worklet')) : Promise.resolve())),
  };
  close = vi.fn(() => {
    this.state = 'closed';
    return Promise.resolve();
  });
  createMediaStreamSource = vi.fn(() => new FakeSourceNode());
  createGain = vi.fn(() => new FakeGainNode());

  constructor() {
    FakeAudioContext.instances.push(this);
  }
}

let track: { stop: ReturnType<typeof vi.fn> };
let getUserMedia: ReturnType<typeof vi.fn>;

beforeEach(() => {
  FakeAudioWorkletNode.instances = [];
  FakeAudioContext.instances = [];
  FakeAudioContext.failAddModule = false;
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('AudioWorkletNode', FakeAudioWorkletNode);
  track = { stop: vi.fn() };
  getUserMedia = vi.fn(() => Promise.resolve({ getTracks: () => [track] }));
  Object.defineProperty(navigator, 'mediaDevices', { value: { getUserMedia }, configurable: true });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('resampleLinear (pure reference copy of the worklet processor code)', () => {
  it('returns the input unchanged when rates match', () => {
    const input = new Float32Array([0.1, 0.2, 0.3]);
    expect(resampleLinear(input, 24000, 24000)).toBe(input);
  });

  it('downsamples to the expected output length', () => {
    expect(resampleLinear(new Float32Array(100), 48000, 24000).length).toBe(50);
  });

  it('linearly interpolates between adjacent samples', () => {
    expect(resampleLinear(new Float32Array([0, 10]), 2, 1)[0]).toBeCloseTo(0, 5);
    expect(resampleLinear(new Float32Array([0, 10, 20, 30]), 3, 2)[1]).toBeCloseTo(15, 5);
  });
});

describe('mapGetUserMediaError', () => {
  it('maps the three getUserMedia failure classes', () => {
    expect(mapGetUserMediaError(new DOMException('x', 'NotAllowedError'))).toBe('not-allowed');
    expect(mapGetUserMediaError(new DOMException('x', 'NotFoundError'))).toBe('audio-capture');
    expect(mapGetUserMediaError(new Error('anything else'))).toBe('network');
  });
});

describe('startSpeechCapture', () => {
  it('requests audio with the expected constraints and wires gain -> worklet -> destination', async () => {
    await startSpeechCapture(() => {});

    expect(getUserMedia).toHaveBeenCalledWith({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    });
    const ctx = FakeAudioContext.instances[0];
    // A same-origin module URL: script-src admits no blob: or data: source.
    const moduleUrl = String(vi.mocked(ctx!.audioWorklet.addModule).mock.calls[0]?.[0]);
    expect(moduleUrl).toMatch(/audioChunkProcessor\.worklet\.js/);
    expect(moduleUrl).not.toMatch(/^(blob|data):/);
    const worklet = FakeAudioWorkletNode.instances[0];
    expect(worklet?.name).toBe(AUDIO_CHUNK_PROCESSOR_NAME);
    expect(worklet?.connect).toHaveBeenCalledWith(ctx?.destination);
  });

  it('asks the worklet for 300 ms frames resampled to 24 kHz from the device rate', async () => {
    await startSpeechCapture(() => {});
    expect(FakeAudioWorkletNode.instances[0]?.port.postMessage).toHaveBeenCalledWith({
      bufferSize: SPEECH_FRAME_SAMPLES,
      inputRate: 44100,
      outputRate: SPEECH_SAMPLE_RATE,
    });
  });

  it('forwards every worklet frame to onFrame', async () => {
    const onFrame = vi.fn();
    await startSpeechCapture(onFrame);
    const frame = new Float32Array([0.1, 0.2]);
    FakeAudioWorkletNode.instances[0]?.port.onmessage?.({ data: frame } as MessageEvent<Float32Array>);
    expect(onFrame).toHaveBeenCalledWith(frame);
  });

  it('release stops the microphone and closes the context, once', async () => {
    const capture = await startSpeechCapture(() => {});
    capture.release();
    capture.release();
    expect(track.stop).toHaveBeenCalledOnce();
    expect(FakeAudioContext.instances[0]?.close).toHaveBeenCalledOnce();
    expect(FakeAudioWorkletNode.instances[0]?.disconnect).toHaveBeenCalledOnce();
  });

  it('a worklet failure releases the microphone and rejects', async () => {
    FakeAudioContext.failAddModule = true;
    await expect(startSpeechCapture(() => {})).rejects.toThrow('no worklet');
    expect(track.stop).toHaveBeenCalledOnce();
    expect(FakeAudioContext.instances[0]?.close).toHaveBeenCalledOnce();
  });

  it('a getUserMedia refusal rejects before any audio graph is built', async () => {
    getUserMedia.mockRejectedValueOnce(new DOMException('denied', 'NotAllowedError'));
    await expect(startSpeechCapture(() => {})).rejects.toThrow('denied');
    expect(FakeAudioContext.instances).toHaveLength(0);
  });
});
