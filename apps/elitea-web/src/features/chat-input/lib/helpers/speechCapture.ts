/**
 * Microphone capture for server speech recognition: getUserMedia ->
 * AudioContext -> AudioWorklet (a processor loaded from a same-origin file)
 * -> fixed-size mono frames at 24 kHz. Ported from the old app's
 * `useStreamingSpeechRecognition.hooks.js`; the AudioWorklet processor code
 * (linear-interpolation resampling, buffered chunking) is byte-for-byte that
 * file's. Only the frame size is fixed now: every model goes through the
 * batch transcription route, so every session uses the old whisper frame
 * (300 ms), which is also what the silence segmenter counts in.
 */

/**
 * The AudioWorklet processor (audioChunkProcessor.worklet.js), as a
 * same-origin URL. `no-inline` keeps Vite from turning the small file into a
 * data: URL, which script-src would refuse just as it refuses a Blob URL.
 */
import audioChunkProcessorUrl from './audioChunkProcessor.worklet.js?url&no-inline';

/** The sample rate frames arrive at, and the rate the WAV upload declares. */
export const SPEECH_SAMPLE_RATE = 24000;
/** 300 ms at 24 kHz — the old whisper frame; two silent frames end an utterance. */
export const SPEECH_FRAME_SAMPLES = 7200;

export const AUDIO_CHUNK_PROCESSOR_NAME = 'audio-chunk-processor';

/** Pure reference copy of audioChunkProcessor.worklet.js's `_resample`; the two must be kept in sync by hand. Exported for direct unit testing only. */
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
    await audioContext.audioWorklet.addModule(audioChunkProcessorUrl);

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
