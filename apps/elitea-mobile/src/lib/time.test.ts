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
    expect(formatAge('2026-09-01T12:00:00Z', now, { locale: 'en-US', timeZone: 'UTC' })).toBe('Sep 1');
    expect(formatAge(null, now)).toBe('');
  });
});

describe('formatAge after a week: the local calendar date (S4)', () => {
  const now = Date.UTC(2026, 9, 4, 12);

  it("is the day in the user's time zone, not the UTC day, either side of midnight", () => {
    // 21:00 on 25 Sep in New York is 01:00 UTC on 26 Sep.
    expect(formatAge('2026-09-26T01:00:00Z', now, { locale: 'en-US', timeZone: 'America/New_York' })).toBe('Sep 25');
    // 08:00 on 26 Sep in Sydney is 22:00 UTC on 25 Sep.
    expect(formatAge('2026-09-25T22:00:00Z', now, { locale: 'en-US', timeZone: 'Australia/Sydney' })).toBe('Sep 26');
  });

  it("follows the user's locale, and shows the year only when it is not this year", () => {
    expect(formatAge('2026-09-25T12:00:00Z', now, { locale: 'en-GB', timeZone: 'UTC' })).toMatch(/^25 Sep/);
    expect(formatAge('2026-09-25T12:00:00Z', now, { locale: 'de-DE', timeZone: 'UTC' })).toMatch(/^25\. Sep/);
    expect(formatAge('2025-12-30T12:00:00Z', now, { locale: 'en-US', timeZone: 'UTC' })).toBe('Dec 30, 2025');
  });

  it('never an ISO date', () => {
    expect(formatAge('2026-09-01T12:00:00Z', now)).not.toMatch(/^\d{4}-\d{2}-\d{2}$/);
  });
});
