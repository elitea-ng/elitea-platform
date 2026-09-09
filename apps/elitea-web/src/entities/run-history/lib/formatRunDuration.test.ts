import { describe, expect, it } from 'vitest';

import { formatRunDuration } from './formatRunDuration';

describe('formatRunDuration', () => {
  it('renders seconds only under a minute', () => {
    expect(formatRunDuration('2026-01-01T00:00:00.000000', '2026-01-01T00:00:42.000000')).toBe('42s');
  });

  it('renders minutes and seconds at and above a minute', () => {
    expect(formatRunDuration('2026-01-01T00:00:00.000000', '2026-01-01T00:01:05.000000')).toBe('1m 5s');
    expect(formatRunDuration('2026-01-01T00:00:00.000000', '2026-01-01T00:02:00.000000')).toBe('2m 0s');
  });

  it('renders 0s for a zero-length span', () => {
    expect(formatRunDuration('2026-01-01T00:00:00.000000', '2026-01-01T00:00:00.000000')).toBe('0s');
  });

  it('falls back to the placeholder when either timestamp is missing', () => {
    expect(formatRunDuration(undefined, '2026-01-01T00:00:00.000000')).toBe('—');
    expect(formatRunDuration('2026-01-01T00:00:00.000000', undefined)).toBe('—');
    expect(formatRunDuration(undefined, undefined)).toBe('—');
  });

  it('falls back to the placeholder for unparsable timestamps', () => {
    expect(formatRunDuration('not-a-date', '2026-01-01T00:00:00.000000')).toBe('—');
    expect(formatRunDuration('2026-01-01T00:00:00.000000', 'not-a-date')).toBe('—');
  });

  it('falls back to the placeholder when updated_at predates created_at (clock skew)', () => {
    expect(formatRunDuration('2026-01-01T00:01:00.000000', '2026-01-01T00:00:00.000000')).toBe('—');
  });
});
