import { describe, expect, it } from 'vitest';

import { fmtDuration, fmtNum, fmtShare, fmtUsd, UNAVAILABLE_METRIC } from './format';

describe('fmtNum', () => {
  it.each([
    [null, '0'],
    [undefined, '0'],
    [0, '0'],
    [1, '1'],
    [999, '999'],
    [1_000, '1.0K'],
    [1_500, '1.5K'],
    [999_999, '1000.0K'],
    [1_000_000, '1.0M'],
    [2_500_000, '2.5M'],
  ])('fmtNum(%p) === %p', (input, expected) => {
    expect(fmtNum(input)).toBe(expected);
  });
});

describe('fmtDuration', () => {
  it.each([
    [null, '-'],
    [undefined, '-'],
    [0, '0ms'],
    [999, '999ms'],
    [1000, '1.0s'],
    [1500, '1.5s'],
    [12345, '12.3s'],
  ])('fmtDuration(%p) === %p', (input, expected) => {
    expect(fmtDuration(input)).toBe(expected);
  });
});

describe('UNAVAILABLE_METRIC', () => {
  it('is a single em dash, distinct from any real formatted number', () => {
    expect(UNAVAILABLE_METRIC).toBe('–');
    expect(UNAVAILABLE_METRIC).not.toBe(fmtNum(0));
  });
});

describe('fmtUsd', () => {
  // The digit COUNT is what these assert, not the currency symbol. The symbol
  // is the runtime locale's ("$" under en-US, "US$" under en-CA), and pinning
  // it makes a suite pass or fail on the machine rather than on the code —
  // which is exactly what `AnalyticsContainer.test.tsx`'s cost-tile case does.
  it.each([
    [null, '0.00'],
    [undefined, '0.00'],
    [0, '0.00'],
    [12.5, '12.50'],
    [1234.5678, '1,234.57'],
  ])('fmtUsd(%p) prints %p at or above one cent', (input, expected) => {
    expect(fmtUsd(input)).toContain(expected);
  });

  // Below one cent, two digits render every figure as zero — which reads as
  // "this project spent nothing" on a screen whose whole subject is spend.
  it.each([
    [0.001952, '0.001952'],
    [0.000189, '0.000189'],
  ])('fmtUsd(%p) keeps six digits below one cent', (input, expected) => {
    expect(fmtUsd(input)).toContain(expected);
  });

  it('does not print a sub-cent amount as zero', () => {
    expect(fmtUsd(0.000189).endsWith('0.00')).toBe(false);
  });
});

describe('fmtShare', () => {
  it.each([
    [1, 4, '25.0%'],
    [2393, 2393, '100.0%'],
    [1, 3, '33.3%'],
  ])('fmtShare(%p, %p) === %p', (value, total, expected) => {
    expect(fmtShare(value, total)).toBe(expected);
  });

  // A share of nothing is not zero percent. Printing 0.0% invites a reader to
  // compare rows that have no denominator between them.
  it.each([
    [0, 0],
    [5, 0],
    [5, -1],
  ])('fmtShare(%p, %p) reports the metric as unavailable', (value, total) => {
    expect(fmtShare(value, total)).toBe(UNAVAILABLE_METRIC);
  });
});
