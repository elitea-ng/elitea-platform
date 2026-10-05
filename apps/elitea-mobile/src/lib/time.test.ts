import { formatAge, parseServerTime } from '@/lib/time';

describe('parseServerTime', () => {
  it('reads RFC 3339 and zone-less server timestamps (as UTC)', () => {
    expect(parseServerTime('2026-10-04T12:00:00Z')).toBe(Date.UTC(2026, 9, 4, 12));
    expect(parseServerTime('2026-10-04T14:00:00+02:00')).toBe(Date.UTC(2026, 9, 4, 12));
    expect(parseServerTime('2026-10-04 12:00:00.123456')).toBe(Date.UTC(2026, 9, 4, 12, 0, 0, 123));
  });

  it('returns null for garbage', () => {
    expect(parseServerTime('yesterday')).toBeNull();
    expect(parseServerTime(42)).toBeNull();
    expect(parseServerTime('')).toBeNull();
  });
});

describe('formatAge', () => {
  const now = Date.UTC(2026, 9, 4, 12);
  it('formats short ages and falls back to a date', () => {
    expect(formatAge('2026-10-04T11:59:30Z', now)).toBe('now');
    expect(formatAge('2026-10-04T11:55:00Z', now)).toBe('5m');
    expect(formatAge('2026-10-04T09:00:00Z', now)).toBe('3h');
    expect(formatAge('2026-10-02T12:00:00Z', now)).toBe('2d');
    expect(formatAge('2026-09-01T12:00:00Z', now)).toBe('2026-09-01');
    expect(formatAge(null, now)).toBe('');
  });
});
