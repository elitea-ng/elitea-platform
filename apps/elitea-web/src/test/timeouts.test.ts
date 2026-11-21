import { afterEach, describe, expect, it, vi } from 'vitest';

import { parseTimeoutScale, testTimeout } from './timeouts';

afterEach(() => {
  vi.unstubAllEnvs();
});

describe('parseTimeoutScale', () => {
  it.each([
    ['3', 3],
    ['1.5', 1.5],
    [' 2 ', 2],
  ])('reads %j as %d', (raw, scale) => {
    expect(parseTimeoutScale(raw)).toBe(scale);
  });

  it.each([undefined, '', '  ', 'fast', 'NaN', 'Infinity', '0', '0.5', '-2', 3])(
    'ignores %j and reads it as 1',
    (raw) => {
      expect(parseTimeoutScale(raw)).toBe(1);
    },
  );
});

describe('testTimeout', () => {
  it('returns the budget unchanged when no scale is set', () => {
    vi.stubEnv('VITEST_TIMEOUT_SCALE', '');
    expect(testTimeout(30_000)).toBe(30_000);
  });

  it('multiplies the budget by the scale the config passed down', () => {
    vi.stubEnv('VITEST_TIMEOUT_SCALE', '3');
    expect(testTimeout(20_000)).toBe(60_000);
  });

  it('rounds a fractional result to whole milliseconds', () => {
    vi.stubEnv('VITEST_TIMEOUT_SCALE', '1.5');
    expect(testTimeout(1_001)).toBe(1_502);
  });

  it('ignores a garbage scale', () => {
    vi.stubEnv('VITEST_TIMEOUT_SCALE', 'lots');
    expect(testTimeout(15_000)).toBe(15_000);
  });
});
