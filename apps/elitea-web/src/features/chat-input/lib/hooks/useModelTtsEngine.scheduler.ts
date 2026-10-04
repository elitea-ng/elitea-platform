/**
 * The Web Audio scheduling engine slice of the model TTS backend
 * (`useTextToSpeech.hooks.js:187-367`) — AudioContext lifecycle, the
 * buffered-playback PCM queue, and the 25ms scheduler loop. Split out of
 * `useModelTtsEngine.hooks.ts` (plain functions operating on the shared
 * `ModelTtsRefs` bag, not hooks themselves) to keep that file under the
 * §3.5 400-line/complexity-12 budgets.
 */
import type { ModelTtsRefs, PendingPcmChunk } from './useModelTtsEngine.types';

/** Match the AudioContext's sample rate to the incoming audio to eliminate sample-rate-conversion artifacts; resume immediately to satisfy the browser autoplay policy. Does NOT auto-resume an already-open (possibly user-paused) context. */
export function ensureAudioContext(refs: ModelTtsRefs, initialVolume: number, sampleRate = 24000): AudioContext {
  const existing = refs.audioContext.current;
  if (existing && existing.state !== 'closed') return existing;

  const ctx = new AudioContext({ sampleRate });
  refs.audioContext.current = ctx;
  refs.nextStartTime.current = 0;
  refs.scheduledSources.current = [];

  const masterGain = ctx.createGain();
  masterGain.gain.value = initialVolume;
  masterGain.connect(ctx.destination);
  refs.masterGain.current = masterGain;

  if (ctx.state === 'suspended') void ctx.resume().catch(() => {});
  return ctx;
}

export function stopModelAudio(refs: ModelTtsRefs): void {
  if (refs.raf.current !== null) {
    cancelAnimationFrame(refs.raf.current);
    refs.raf.current = null;
  }
  for (const src of refs.scheduledSources.current) {
    try {
      src.stop();
    } catch {
      // Handled (§3.6): already stopped.
    }
  }
  refs.scheduledSources.current = [];
  if (refs.audioContext.current && refs.audioContext.current.state !== 'closed') {
    void refs.audioContext.current.close();
  }
  refs.audioContext.current = null;
  refs.masterGain.current = null;
  refs.nextStartTime.current = 0;
  refs.playStartTime.current = null;
  refs.totalDuration.current = 0;
  refs.allChunksReceived.current = false;
  refs.charTimeline.current = null;
  refs.sentenceWaypoints.current = [];
  if (refs.schedulerTimer.current !== null) clearInterval(refs.schedulerTimer.current);
  refs.schedulerTimer.current = null;
  refs.pcmQueue.current = [];
  refs.finalTtsDone.current = false;
  refs.totalEnqueuedSamples.current = 0;
}

/**
 * Push decoded samples into the playback queue. Sub-render-quantum (< 128
 * sample) fragments are merged into the previous queue entry instead of
 * being scheduled as their own `AudioBufferSourceNode` — HTTP chunked-
 * transfer occasionally produces 1-4 byte trailing fragments at sentence
 * boundaries, which pop audibly if scheduled as standalone buffers.
 */
export function enqueueSamples(refs: ModelTtsRefs, samples: Float32Array<ArrayBuffer>, sampleRate = 24000): void {
  refs.sampleRate.current = sampleRate;
  const QUANTUM = 128;
  const queue = refs.pcmQueue.current;
  const last = queue[queue.length - 1];
  if (samples.length < QUANTUM && last) {
    const merged = new Float32Array(last.samples.length + samples.length);
    merged.set(last.samples, 0);
    merged.set(samples, last.samples.length);
    queue[queue.length - 1] = { samples: merged, sampleRate };
  } else {
    queue.push({ samples, sampleRate });
  }
  refs.totalEnqueuedSamples.current += samples.length;
}

function schedulePendingSegments(refs: ModelTtsRefs, ctx: AudioContext, scheduleUntil: number): void {
  while (refs.nextStartTime.current < scheduleUntil) {
    const segment: PendingPcmChunk | undefined = refs.pcmQueue.current.shift();
    if (!segment) break;
    try {
      const buffer = ctx.createBuffer(1, segment.samples.length, segment.sampleRate);
      buffer.copyToChannel(segment.samples, 0);
      const source = ctx.createBufferSource();
      source.buffer = buffer;
      source.connect(refs.masterGain.current ?? ctx.destination);

      const startTime = Math.max(refs.nextStartTime.current, ctx.currentTime);
      if (refs.playStartTime.current === null) refs.playStartTime.current = startTime;
      source.start(startTime);
      refs.nextStartTime.current = startTime + buffer.duration;

      refs.scheduledSources.current.push(source);
      source.onended = () => {
        refs.scheduledSources.current = refs.scheduledSources.current.filter((s) => s !== source);
      };
    } catch (err) {
      // eslint-disable-next-line no-console -- deliberate: a per-chunk scheduling failure is logged, not thrown (§3.6).
      console.error('[TTS] Failed to schedule PCM chunk:', err);
    }
  }
}

/** Fades the master gain to 0 over the last 20ms once the stream is fully drained, to silence the DC-offset click a PCM buffer ending at a non-zero sample would otherwise produce. Also records the measured duration and recalibrates `calibratedRate` for the next session. */
function finishStreamIfDrained(refs: ModelTtsRefs, ctx: AudioContext): void {
  if (!(refs.finalTtsDone.current && refs.pcmQueue.current.length === 0 && !refs.allChunksReceived.current)) return;

  const bufferedDuration = refs.playStartTime.current !== null ? refs.nextStartTime.current - refs.playStartTime.current : 0;
  refs.totalDuration.current = bufferedDuration > 0 ? bufferedDuration : 0;
  if (bufferedDuration > 0 && refs.fullText.current.length > 0) {
    refs.calibratedRate.current = refs.fullText.current.length / bufferedDuration;
  }

  const masterGain = refs.masterGain.current;
  if (masterGain && refs.nextStartTime.current > 0) {
    const FADE_OUT = 0.02;
    const endTime = refs.nextStartTime.current;
    masterGain.gain.setValueAtTime(1, Math.max(ctx.currentTime, endTime - FADE_OUT));
    masterGain.gain.linearRampToValueAtTime(0, endTime);
  }
  refs.allChunksReceived.current = true;
}

/**
 * Scheduler tick — invoked every 25ms. Pre-schedules queued PCM into the
 * AudioContext up to `LOOKAHEAD` seconds ahead of `ctx.currentTime`, holding
 * off the first buffer until `PREROLL_SAMPLES` (200ms) is buffered so
 * transient network jitter cannot immediately cause an underrun.
 */
export function scheduleFromQueue(refs: ModelTtsRefs): void {
  const ctx = refs.audioContext.current;
  if (!ctx || ctx.state === 'closed') return;

  if (ctx.state === 'suspended' && !refs.userPaused.current) {
    void ctx.resume().catch(() => {});
    return;
  }

  const sr = refs.sampleRate.current;
  const PREROLL_SAMPLES = Math.floor(sr * 0.2);
  const LOOKAHEAD = 0.25;

  if (refs.playStartTime.current === null) {
    const queued = refs.pcmQueue.current.reduce((sum, seg) => sum + seg.samples.length, 0);
    if (queued < PREROLL_SAMPLES && !refs.finalTtsDone.current) return;
  }

  schedulePendingSegments(refs, ctx, ctx.currentTime + LOOKAHEAD);
  finishStreamIfDrained(refs, ctx);
}
