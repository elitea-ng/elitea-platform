import { describe, expect, it } from 'vitest';

import { applyFade } from './ttsPcm.helpers';

describe('applyFade', () => {
  it('fades in from 0 up to (near) full amplitude over fadeSamples', () => {
    const samples = new Float32Array([1, 1, 1, 1]);
    applyFade(samples, 4, 'in');
    expect(samples[0]).toBeCloseTo(0);
    expect(samples[1]).toBeCloseTo(0.25);
    expect(samples[2]).toBeCloseTo(0.5);
    expect(samples[3]).toBeCloseTo(0.75);
  });

  it('fades out from (near) full amplitude down toward 0 over fadeSamples', () => {
    const samples = new Float32Array([1, 1, 1, 1]);
    applyFade(samples, 4, 'out');
    expect(samples[0]).toBeCloseTo(0.75);
    expect(samples[1]).toBeCloseTo(0.5);
    expect(samples[2]).toBeCloseTo(0.25);
    expect(samples[3]).toBeCloseTo(0);
  });

  it('only touches the last fadeSamples entries when fading out a longer buffer', () => {
    const samples = new Float32Array([2, 2, 2, 2, 2]);
    applyFade(samples, 2, 'out');
    expect(samples[0]).toBeCloseTo(2);
    expect(samples[1]).toBeCloseTo(2);
    expect(samples[2]).toBeCloseTo(2);
    expect(samples[3]).toBeCloseTo(1); // (2-1-0)/2 * 2 = 1
    expect(samples[4]).toBeCloseTo(0); // (2-1-1)/2 * 2 = 0
  });

  it('clamps fadeSamples to the buffer length when it exceeds it', () => {
    const samples = new Float32Array([1, 1]);
    expect(() => applyFade(samples, 100, 'in')).not.toThrow();
    expect(samples[0]).toBeCloseTo(0);
    expect(samples[1]).toBeCloseTo(0.5);
  });
});
