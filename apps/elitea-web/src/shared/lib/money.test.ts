import { describe, expect, it } from 'vitest';

import { formatSmallUsd } from './money';

// Digit strings, not the currency symbol: the symbol is the runtime locale's
// ("$" under en-US, "US$" under en-CA). See analytics `format.test.ts`.
describe('formatSmallUsd (#6682)', () => {
  it('prints a real zero as 0.00', () => {
    expect(formatSmallUsd(0)).toContain('0.00');
    expect(formatSmallUsd(0)).not.toContain('<');
  });

  it('never prints a sub-micro-dollar amount as zero', () => {
    const out = formatSmallUsd(8e-8);
    expect(out.startsWith('< ')).toBe(true);
    expect(out).toContain('0.000001');
  });

  it('keeps the sign of a tiny negative amount', () => {
    const out = formatSmallUsd(-8e-8);
    expect(out.startsWith('> ')).toBe(true);
    expect(out).toContain('0.000001');
  });

  it.each([
    [0.004, '0.004'],
    [0.000189, '0.000189'],
    [0.000001, '0.000001'],
    [0.0099, '0.0099'],
  ])('keeps up to six digits below one cent: %p → %p', (input, expected) => {
    expect(formatSmallUsd(input)).toContain(expected);
    expect(formatSmallUsd(input)).not.toContain('<');
  });

  it.each([
    [0.01, '0.01'],
    [12.5, '12.50'],
    [1234.5678, '1,234.57'],
  ])('keeps two digits from one cent up: %p → %p', (input, expected) => {
    expect(formatSmallUsd(input)).toContain(expected);
  });

  it.each([Number.NaN, Number.POSITIVE_INFINITY])('prints a non-finite %p as zero', (input) => {
    expect(formatSmallUsd(input)).toContain('0.00');
  });
});
