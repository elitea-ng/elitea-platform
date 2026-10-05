/**
 * Microphone capture for server speech recognition: getUserMedia ->
 * AudioContext -> AudioWorklet (an inline processor registered from a Blob
 * URL) -> fixed-size mono frames at 24 kHz. Ported from the old app's
 * `useStreamingSpeechRecognition.hooks.js`; the AudioWorklet processor code
 * (linear-interpolation resampling, buffered chunking) is byte-for-byte that
 * file's. Only the frame size is fixed now: every model goes through the
 * batch transcription route, so every session uses the old whisper frame
 * (300 ms), which is also what the silence segmenter counts in.
 */

/** The sample rate frames arrive at, and the rate the WAV upload declares. */
export const SPEECH_SAMPLE_RATE = 24000;
/** 300 ms at 24 kHz — the old whisper frame; two silent frames end an utterance. */
export const SPEECH_FRAME_SAMPLES = 7200;

export const AUDIO_CHUNK_PROCESSOR_NAME = 'audio-chunk-processor';

/**
 * AudioWorklet processor source — registered from a Blob URL and executed in
 * the AudioWorkletGlobalScope (a separate JS realm; it cannot `import` app
 * code, hence this being a plain string, byte-for-byte ported from the old
 * app). Not directly unit-testable in jsdom (no AudioWorklet runtime). The
 * resample math is mirrored by the pure, exported {@link resampleLinear}
 * below purely so it can be unit-tested directly — the two must be kept in
 * sync by hand if either ever changes; the worklet copy below is what
 * actually runs in production.
 */
const AUDIO_CHUNK_PROCESSOR_CODE = `
class AudioChunkProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this._buffer = [];
    this._bufferSize = 4800;    // output samples at TARGET_SAMPLE_RATE
    this._inputRate = 44100;    // overridden via port message
    this._outputRate = 24000;   // overridden via port message
    this.port.onmessage = (e) => {
      if (e.data?.bufferSize)  this._bufferSize  = e.data.bufferSize;
      if (e.data?.inputRate)   this._inputRate   = e.data.inputRate;
      if (e.data?.outputRate)  this._outputRate  = e.data.outputRate;
    };
  }

  _resample(input) {
    if (this._inputRate === this._outputRate) return input;
    const ratio = this._inputRate / this._outputRate;
    const outLen = Math.round(input.length / ratio);
    const out = new Float32Array(outLen);
    for (let i = 0; i < outLen; i++) {
      const src = i * ratio;
      const lo = Math.floor(src);
      const hi = Math.min(lo + 1, input.length - 1);
      out[i] = input[lo] + (input[hi] - input[lo]) * (src - lo);
    }
    return out;
  }

  process(inputs) {
    const channel = inputs[0]?.[0];
    if (!channel) return true;

    const resampled = this._resample(channel);
    for (let i = 0; i < resampled.length; i++) {
      this._buffer.push(resampled[i]);
    }

    while (this._buffer.length >= this._bufferSize) {
      const chunk = new Float32Array(this._buffer.splice(0, this._bufferSize));
      this.port.postMessage(chunk);
    }

    return true;
  }
}

registerProcessor('audio-chunk-processor', AudioChunkProcessor);
`;

/** Pure reference copy of {@link AUDIO_CHUNK_PROCESSOR_CODE}'s `_resample` — see that constant's doc comment. Exported for direct unit testing only. */
export function resampleLinear(input: Float32Array, inputRate: number, outputRate: number): Float32Array {
  if (inputRate === outputRate) return input;
  const ratio = inputRate / outputRate;
  const outLen = Math.round(input.length / ratio);
  const out = new Float32Array(outLen);
  for (let i = 0; i < outLen; i++) {
    const src = i * ratio;
    const lo = Math.floor(src);
    const hi = Math.min(lo + 1, input.length - 1);
    const loVal = input[lo] ?? 0;
    const hiVal = input[hi] ?? 0;
    out[i] = loVal + (hiVal - loVal) * (src - lo);
  }
  return out;
}

/** The three `getUserMedia` failure classes the mic UI words differently. */
export function mapGetUserMediaError(err: unknown): 'not-allowed' | 'audio-capture' | 'network' {
  const name = err instanceof DOMException ? err.name : undefined;
  if (name === 'NotAllowedError') return 'not-allowed';
  if (name === 'NotFoundError') return 'audio-capture';
  return 'network';
}

export interface SpeechCapture {
  /** Stops the microphone and closes the audio graph. Safe to call twice. */
  readonly release: () => void;
}

/** Opens the microphone and calls `onFrame` with each 24 kHz mono frame. Rejects with the `getUserMedia`/AudioWorklet failure. */
export async function startSpeechCapture(onFrame: (frame: Float32Array) => void): Promise<SpeechCapture> {
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
  });
  // The browser's native rate avoids cross-rate errors (e.g. Firefox); the
  // worklet resamples to SPEECH_SAMPLE_RATE.
  const audioContext = new AudioContext();
  let released = false;
  let worklet: AudioWorkletNode | null = null;
  const release = (): void => {
    if (released) return;
    released = true;
    worklet?.disconnect();
    if (audioContext.state !== 'closed') void audioContext.close();
    stream.getTracks().forEach((track) => track.stop());
  };
  try {
    const blobUrl = URL.createObjectURL(new Blob([AUDIO_CHUNK_PROCESSOR_CODE], { type: 'application/javascript' }));
    await audioContext.audioWorklet.addModule(blobUrl);
    URL.revokeObjectURL(blobUrl);

    const source = audioContext.createMediaStreamSource(stream);
    worklet = new AudioWorkletNode(audioContext, AUDIO_CHUNK_PROCESSOR_NAME);
    worklet.port.postMessage({ bufferSize: SPEECH_FRAME_SAMPLES, inputRate: audioContext.sampleRate, outputRate: SPEECH_SAMPLE_RATE });
    worklet.port.onmessage = (event: MessageEvent<Float32Array>) => onFrame(event.data);

    const gain = audioContext.createGain();
    gain.gain.value = 0.7; // 1.0 = unity, >1.0 = boost, <1.0 = attenuate
    source.connect(gain);
    gain.connect(worklet);
    worklet.connect(audioContext.destination);
  } catch (err) {
    release();
    throw err;
  }
  return { release };
}
