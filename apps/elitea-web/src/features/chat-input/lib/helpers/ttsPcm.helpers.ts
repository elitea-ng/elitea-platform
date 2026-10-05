/**
 * Ported from `useTextToSpeech.hooks.js:545-560` — the linear fade-in/out
 * `useModelTtsEngine.stream.ts` applies at every sentence boundary. Pure — no
 * AudioContext/DOM dependency — split out for independent unit testing. The
 * PCM16 decoder that sat beside it left with the socket.io transport: the
 * HTTP speech route answers a whole audio file, which `decodeAudioData` reads.
 */

/**
 * Applies a linear fade-in or fade-out over `fadeSamples` samples IN PLACE.
 * Used to eliminate amplitude-discontinuity "pops" at sentence boundaries
 * (fade-out the tail of one sentence's audio, fade-in the head of the next).
 */
export function applyFade(samples: Float32Array<ArrayBuffer>, fadeSamples: number, type: 'in' | 'out'): void {
  const count = Math.min(fadeSamples, samples.length);
  if (type === 'in') {
    for (let i = 0; i < count; i++) {
      samples[i] = (samples[i] ?? 0) * (i / count);
    }
  } else {
    const start = samples.length - count;
    for (let i = 0; i < count; i++) {
      const idx = start + i;
      samples[idx] = (samples[idx] ?? 0) * ((count - 1 - i) / count);
    }
  }
}
