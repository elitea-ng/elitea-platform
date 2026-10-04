import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { presetToDateRange, toIsoRange } from './dateRange';

/**
 * Pinned to a zone WITH daylight saving so the DST cases mean something on
 * every machine (CI runs UTC, which has none). Node re-reads `TZ` on change.
 */
const ZONE = 'Europe/Berlin';
let previousTz: string | undefined;

beforeAll(() => {
  previousTz = process.env.TZ;
  process.env.TZ = ZONE;
});

afterAll(() => {
  if (previousTz === undefined) delete process.env.TZ;
  else process.env.TZ = previousTz;
});

/** A local wall-clock instant in `ZONE`. */
function local(y: number, m: number, d: number, h = 0, min = 0, s = 0, ms = 0): Date {
  return new Date(y, m - 1, d, h, min, s, ms);
}

function wall(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}.${String(date.getMilliseconds()).padStart(3, '0')}`;
}

/** Calendar days covered by [from, to], counted on the local calendar. */
function calendarDays(from: Date, to: Date): number {
  let n = 0;
  const cursor = new Date(from);
  while (cursor.getTime() <= to.getTime()) {
    n += 1;
    cursor.setDate(cursor.getDate() + 1);
  }
  return n;
}

it('runs in a zone with daylight saving (guard for the DST cases)', () => {
  expect(new Date(2026, 0, 1).getTimezoneOffset()).not.toBe(new Date(2026, 6, 1).getTimezoneOffset());
});

describe('presetToDateRange — calendar-day presets (#6791)', () => {
  const now = local(2026, 9, 28, 14, 37, 12, 345);

  it('Today is today 00:00 .. 23:59, whatever the time of day', () => {
    const { from, to } = presetToDateRange(1, now);
    expect(wall(from)).toBe('2026-09-28 00:00:00.000');
    expect(wall(to)).toBe('2026-09-28 23:59:00.000');
  });

  it('Last 7d is 22 Sep 00:00 .. 28 Sep 23:59 (the issue example)', () => {
    const { from, to } = presetToDateRange(7, now);
    expect(wall(from)).toBe('2026-09-22 00:00:00.000');
    expect(wall(to)).toBe('2026-09-28 23:59:00.000');
  });

  it.each([1, 7, 30, 90])('Last %i covers exactly that many calendar days, today included', (days) => {
    const { from, to } = presetToDateRange(days, now);
    expect(calendarDays(from, to)).toBe(days);
    expect(from.getHours()).toBe(0);
    expect(from.getMinutes()).toBe(0);
  });

  it('does not depend on the time of day: 00:00:00 and 23:59:59 give the same range', () => {
    const early = presetToDateRange(30, local(2026, 9, 28, 0, 0, 0, 0));
    const late = presetToDateRange(30, local(2026, 9, 28, 23, 59, 59, 999));
    expect(early.from.getTime()).toBe(late.from.getTime());
    expect(early.to.getTime()).toBe(late.to.getTime());
  });

  it('rides over a month boundary', () => {
    const { from } = presetToDateRange(7, local(2026, 3, 3, 9));
    expect(wall(from)).toBe('2026-02-25 00:00:00.000');
  });

  it('rides over a leap-year February', () => {
    const { from } = presetToDateRange(7, local(2028, 3, 2, 9));
    expect(wall(from)).toBe('2028-02-25 00:00:00.000');
  });

  it('rides over a year boundary', () => {
    const { from, to } = presetToDateRange(30, local(2027, 1, 10, 9));
    expect(wall(from)).toBe('2026-12-12 00:00:00.000');
    expect(wall(to)).toBe('2027-01-10 23:59:00.000');
  });

  it('keeps From at local midnight across the spring-forward change (29 Mar 2026)', () => {
    const { from, to } = presetToDateRange(7, local(2026, 4, 2, 12));
    expect(wall(from)).toBe('2026-03-27 00:00:00.000');
    // 7 calendar days containing a 23-hour day: NOT 7 × 24 h.
    expect(to.getTime() + 60_000 - from.getTime()).toBe((7 * 24 - 1) * 3_600_000);
  });

  it('keeps From at local midnight across the fall-back change (25 Oct 2026)', () => {
    const { from, to } = presetToDateRange(7, local(2026, 10, 28, 12));
    expect(wall(from)).toBe('2026-10-22 00:00:00.000');
    expect(to.getTime() + 60_000 - from.getTime()).toBe((7 * 24 + 1) * 3_600_000);
  });

  it('treats a non-positive day count as Today', () => {
    expect(presetToDateRange(0, now)).toEqual(presetToDateRange(1, now));
  });

  it('does not mutate `now` and returns fresh instances', () => {
    const before = now.getTime();
    const { from, to } = presetToDateRange(7, now);
    expect(now.getTime()).toBe(before);
    expect(from).not.toBe(now);
    expect(to).not.toBe(now);
  });
});

describe('toIsoRange — the To minute is included', () => {
  it('sends a preset as [first midnight, next midnight)', () => {
    const range = presetToDateRange(1, local(2026, 9, 28, 14, 37));
    expect(toIsoRange(range)).toEqual({
      dateFrom: local(2026, 9, 28).toISOString(),
      dateTo: local(2026, 9, 29).toISOString(),
    });
  });

  it('includes the whole final minute: an event at 23:59:59.999 is inside, the next midnight is not', () => {
    const { dateFrom, dateTo } = toIsoRange(presetToDateRange(1, local(2026, 9, 28, 8)));
    const inside = local(2026, 9, 28, 23, 59, 59, 999).getTime();
    const nextMidnight = local(2026, 9, 29).getTime();
    expect(inside >= Date.parse(dateFrom) && inside < Date.parse(dateTo)).toBe(true);
    expect(nextMidnight < Date.parse(dateTo)).toBe(false);
  });

  it('ends a Last 7d window at the next local midnight across the fall-back change', () => {
    const { dateTo } = toIsoRange(presetToDateRange(7, local(2026, 10, 28, 12)));
    expect(dateTo).toBe(local(2026, 10, 29).toISOString());
  });

  it('keeps a Custom range at the picked minutes, not day boundaries', () => {
    const from = local(2026, 9, 27, 14, 30, 41, 500);
    const to = local(2026, 9, 28, 14, 30, 2, 0);
    expect(toIsoRange({ from, to })).toEqual({
      dateFrom: local(2026, 9, 27, 14, 30).toISOString(),
      dateTo: local(2026, 9, 28, 14, 31).toISOString(),
    });
  });

  it('emits ISO 8601 UTC instants', () => {
    const { dateFrom, dateTo } = toIsoRange(presetToDateRange(1, local(2026, 7, 27, 12)));
    expect(dateFrom).toBe('2026-07-26T22:00:00.000Z');
    expect(dateTo).toBe('2026-07-27T22:00:00.000Z');
  });
});
