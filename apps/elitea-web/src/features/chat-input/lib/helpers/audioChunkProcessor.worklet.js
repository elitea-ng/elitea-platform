// AudioWorklet processor for startSpeechCapture (speechCapture.ts). It runs in
// the AudioWorkletGlobalScope, a separate realm that cannot import app code,
// and is loaded as its own same-origin file so the page's script-src needs no
// blob: source. Byte-for-byte the old app's processor. The resample math is
// mirrored by resampleLinear in speechCapture.ts for unit tests; keep the two
// in sync by hand.
// The AudioWorkletGlobalScope globals (AudioWorkletProcessor, registerProcessor)
// have no types in the app's DOM lib, so type-aware rules cannot see them.
/* oxlint-disable typescript/no-unsafe-call, typescript/no-unsafe-member-access, typescript/no-unsafe-assignment, typescript/no-unsafe-return */
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
