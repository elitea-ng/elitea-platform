/**
 * Pure audio helpers for the HTTP voice transport (`api/voiceTransport.ts`).
 *
 * Two pieces of the old socket.io server's work moved into the browser with
 * the transport, and both are ported here byte-for-byte in behaviour:
 *
 *  - The sentence split (`indexer_tts.py`'s `_split_sentences`). The server
 *    synthesised one request per sentence and sent a `tts_done {char_end}`
 *    after each one; the read-aloud highlight interpolates between those
 *    offsets. The browser now makes the per-sentence requests itself.
 *  - The speech segmenter (`sio/asr.py`'s whisper VAD). The server cut the
 *    microphone stream into utterances on silence and sent each one to the
 *    transcription route. The browser now does the cut and the upload.
 */

/** One speech request: the sentence and the offset just after it in the source text. */
export interface SpeechSegment {
  readonly text: string;
  readonly charEnd: number;
}

// Period/exclamation/question followed by whitespace, or one or more newlines
// (`indexer_tts.py`'s `_SENTENCE_BOUNDARY_RE`).
const SENTENCE_BOUNDARY = /(?<=[.!?])\s+|\n+/g;

/** OpenAI's documented limit for one speech request's `input` is 4096 characters. */
const MAX_SPEECH_SEGMENT_CHARS = 4000;

/**
 * Pushes the sentence `text[start, end)` (already trimmed of outer whitespace),
 * cut at the last space before `max` characters when it is longer than that —
 * one speech request must stay inside the provider's input limit.
 */
function pushSentence(text: string, start: number, end: number, max: number, out: SpeechSegment[]): void {
  let from = start;
  while (end - from > max) {
    const space = text.lastIndexOf(' ', from + max);
    const cut = space > from ? space : from + max;
    out.push({ text: text.slice(from, cut).trim(), charEnd: cut });
    from = cut;
    while (from < end && /\s/.test(text.charAt(from))) from += 1;
  }
  if (end > from) out.push({ text: text.slice(from, end), charEnd: end });
}

/** Splits `text` into speech requests; `charEnd` is the offset just after each sentence (before inter-sentence whitespace). */
export function splitSpeechSegments(text: string, max = MAX_SPEECH_SEGMENT_CHARS): SpeechSegment[] {
  const out: SpeechSegment[] = [];
  const push = (from: number, to: number): void => {
    const raw = text.slice(from, to);
    const start = from + (raw.length - raw.trimStart().length);
    const end = from + raw.trimEnd().length;
    if (end > start) pushSentence(text, start, end, max, out);
  };
  let prev = 0;
  for (const match of text.matchAll(SENTENCE_BOUNDARY)) {
    push(prev, match.index);
    prev = match.index + match[0].length;
  }
  push(prev, text.length);
  return out;
}

const GPT4O_TTS_INSTRUCTIONS =
  'Affect: calm and warm. Pacing: steady and measured, never rushing. Tone: conversational and clear. ' +
  'Do not change your speaking style, pitch, or rhythm between sentences.';

/**
 * Per-model `instructions` that keep one voice consistent across the separate
 * per-sentence requests (`indexer_tts.py`'s `_get_tone_params`). Only the
 * gpt-4o TTS family reads the field. The ElevenLabs `previous_text` hint is not
 * ported: the gateway decodes speech into a typed request and drops it.
 */
export function speechInstructionsFor(modelName: string): string | undefined {
  const lower = modelName.toLowerCase();
  return lower.includes('gpt-4o') && lower.includes('tts') ? GPT4O_TTS_INSTRUCTIONS : undefined;
}

/** Float32 [-1, 1] samples to 16-bit little-endian PCM. */
export function float32ToPcm16Buffer(float32Array: Float32Array): ArrayBuffer {
  const pcm = new Int16Array(float32Array.length);
  for (let i = 0; i < float32Array.length; i++) {
    const clamped = Math.max(-1, Math.min(1, float32Array[i] ?? 0));
    pcm[i] = clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff;
  }
  return pcm.buffer;
}

/** Wraps mono samples in a 16-bit PCM WAV file — the body `indexer_asr_whisper.py`'s `_pcm16_to_wav` sent. */
export function encodeWav(samples: Float32Array, sampleRate: number): Blob {
  const pcm = float32ToPcm16Buffer(samples);
  const header = new DataView(new ArrayBuffer(44));
  const ascii = (offset: number, value: string): void => {
    for (let i = 0; i < value.length; i++) header.setUint8(offset + i, value.charCodeAt(i));
  };
  ascii(0, 'RIFF');
  header.setUint32(4, 36 + pcm.byteLength, true);
  ascii(8, 'WAVE');
  ascii(12, 'fmt ');
  header.setUint32(16, 16, true);
  header.setUint16(20, 1, true); // PCM
  header.setUint16(22, 1, true); // mono
  header.setUint32(24, sampleRate, true);
  header.setUint32(28, sampleRate * 2, true);
  header.setUint16(32, 2, true);
  header.setUint16(34, 16, true);
  ascii(36, 'data');
  header.setUint32(40, pcm.byteLength, true);
  return new Blob([header.buffer, pcm], { type: 'audio/wav' });
}

export type SegmenterEvent =
  /** Silence turned into speech — the speaking-mode loop cancels its pending auto-send. */
  | { readonly kind: 'speech-start' }
  /** An utterance ended; its samples go to the transcription route. */
  | { readonly kind: 'segment'; readonly samples: Float32Array }
  /** Speech was detected but too short to transcribe; the caller still balances its pending count. */
  | { readonly kind: 'discard' };

export interface SegmenterOptions {
  /** Peak amplitude (of 1.0) above which a frame is speech. `sio/asr.py`: 500 of 32767. */
  readonly threshold?: number;
  /** Consecutive silent frames that end an utterance. With 300 ms frames, 2 = 600 ms. */
  readonly silenceFrames?: number;
  /** Shorter utterances are discarded, not uploaded. `sio/asr.py`: 4800 bytes = 2400 samples. */
  readonly minSamples?: number;
  /** Longer utterances are cut and uploaded, so one request stays bounded. */
  readonly maxSamples?: number;
}

/**
 * The whisper VAD from `sio/asr.py` (`_handle_whisper_audio` / `_do_flush`),
 * moved client-side. Frames are the worklet's fixed-size chunks.
 */
export class UtteranceSegmenter {
  private readonly threshold: number;
  private readonly silenceFrames: number;
  private readonly minSamples: number;
  private readonly maxSamples: number;
  private frames: Float32Array[] = [];
  private length = 0;
  private speech = false;
  private silent = 0;

  constructor(options: SegmenterOptions = {}) {
    this.threshold = options.threshold ?? 500 / 32768;
    this.silenceFrames = options.silenceFrames ?? 2;
    this.minSamples = options.minSamples ?? 2400;
    this.maxSamples = options.maxSamples ?? 24000 * 30;
  }

  push(frame: Float32Array): SegmenterEvent[] {
    if (frame.length === 0) return [];
    const events: SegmenterEvent[] = [];
    if (isSpeech(frame, this.threshold)) {
      if (!this.speech) events.push({ kind: 'speech-start' });
      this.append(frame);
      this.speech = true;
      this.silent = 0;
      if (this.length >= this.maxSamples) events.push(this.cut());
    } else if (this.speech) {
      // The trailing silent frame stays in the utterance for natural context.
      this.append(frame);
      this.silent += 1;
      if (this.silent >= this.silenceFrames) events.push(this.cut());
    }
    return events;
  }

  /** Ends the utterance in progress, if any (the user stopped recording). */
  flush(): SegmenterEvent[] {
    return this.speech ? [this.cut()] : [];
  }

  private append(frame: Float32Array): void {
    this.frames.push(frame);
    this.length += frame.length;
  }

  private cut(): SegmenterEvent {
    const samples = new Float32Array(this.length);
    let offset = 0;
    for (const frame of this.frames) {
      samples.set(frame, offset);
      offset += frame.length;
    }
    this.frames = [];
    this.length = 0;
    this.speech = false;
    this.silent = 0;
    return samples.length < this.minSamples ? { kind: 'discard' } : { kind: 'segment', samples };
  }
}

function isSpeech(frame: Float32Array, threshold: number): boolean {
  for (const sample of frame) {
    if (Math.abs(sample) > threshold) return true;
  }
  return false;
}
