/**
 * The HTTP speech stream of the model TTS engine: one `POST
 * /llm/v1/audio/speech` per sentence, decoded and queued for gap-free
 * playback. It replaces the socket.io `tts_audio_chunk`/`tts_done`/`tts_error`
 * listeners (`api/voiceTransport.ts` says why).
 *
 * The old server sent one sentence's PCM and then `tts_done {char_end}`. Each
 * HTTP answer is exactly one sentence, so {@link appendSentence} does the same
 * work in one step: fade the sentence in and out (no pop at the boundary),
 * queue it, and record the `char_end` waypoint the highlight interpolates on.
 * The NEXT sentence is requested while the current one decodes, so playback
 * does not wait one round trip per sentence.
 */
import { synthesizeSpeech } from '../../api/voiceTransport';
import type { SpeechRequest } from '../../api/voiceTransport';
import { applyFade } from '../helpers/ttsPcm.helpers';
import { splitSpeechSegments } from '../helpers/voiceAudio.helpers';

import { enqueueSamples } from './useModelTtsEngine.scheduler';
import type { ModelTtsRefs } from './useModelTtsEngine.types';

const FADE_SECONDS = 0.005;

/** Queues one decoded sentence and records its `char_end` highlight waypoint. */
function appendSentence(refs: ModelTtsRefs, samples: Float32Array<ArrayBuffer>, sampleRate: number, charEnd: number): void {
  const fade = Math.floor(sampleRate * FADE_SECONDS);
  applyFade(samples, fade, 'in');
  applyFade(samples, fade, 'out');
  enqueueSamples(refs, samples, sampleRate);
  // From total samples queued, not `nextStartTime`, so the waypoint is right
  // however far the scheduler has got through the queue.
  refs.sentenceWaypoints.current.push({ charPos: charEnd, audioTime: refs.totalEnqueuedSamples.current / sampleRate });
}

/** The last sentence is queued: the scheduler may drain and end the stream. */
function finishSpeech(refs: ModelTtsRefs): void {
  refs.finalTtsDone.current = true;
}

/** Channel 0 of a decoded buffer, copied so the fades do not write into the decoder's memory. */
function monoSamples(buffer: AudioBuffer): Float32Array<ArrayBuffer> {
  const samples = new Float32Array(buffer.length);
  samples.set(buffer.getChannelData(0));
  return samples;
}

export interface SpeechStreamParams {
  readonly refs: ModelTtsRefs;
  readonly text: string;
  readonly request: Omit<SpeechRequest, 'input'>;
  readonly signal: AbortSignal;
}

/**
 * Requests, decodes and queues every sentence of `text` in order. Resolves
 * when the last sentence is queued; rejects with the first failure. The
 * caller aborts `signal` to stop, and an aborted run resolves quietly.
 */
export async function streamSpeech({ refs, text, request, signal }: SpeechStreamParams): Promise<void> {
  const segments = splitSpeechSegments(text);
  const fetchSegment = (index: number): Promise<ArrayBuffer> => {
    const segment = segments[index];
    const pending = segment ? synthesizeSpeech({ ...request, input: segment.text }, signal) : Promise.resolve(new ArrayBuffer(0));
    // Observed now so a prefetch that fails while an earlier sentence plays
    // is not an unhandled rejection; it is still awaited (and thrown) below.
    void pending.catch(() => undefined);
    return pending;
  };

  let next = segments.length > 0 ? fetchSegment(0) : undefined;
  for (let index = 0; index < segments.length && next; index++) {
    const audio = await next;
    next = index + 1 < segments.length ? fetchSegment(index + 1) : undefined;
    const ctx = refs.audioContext.current;
    if (signal.aborted || !ctx || ctx.state === 'closed') return;
    const decoded = await ctx.decodeAudioData(audio);
    if (signal.aborted) return;
    appendSentence(refs, monoSamples(decoded), decoded.sampleRate, segments[index]?.charEnd ?? text.length);
  }
  if (!signal.aborted) finishSpeech(refs);
}
