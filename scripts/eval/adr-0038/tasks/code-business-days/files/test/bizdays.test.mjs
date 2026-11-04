import test from 'node:test';
import assert from 'node:assert/strict';
import { isBusinessDay, addBusinessDays, businessDaysBetween } from '../src/bizdays.mjs';

// Calendar (UTC): 2026-07-02 Thu, 07-03 Fri, 07-04 Sat, 07-05 Sun, 07-06 Mon.
// 2026-12-24 Thu, 12-25 Fri, 12-31 Thu, 2027-01-01 Fri.
const d = (iso) => new Date(iso.length === 10 ? `${iso}T00:00:00Z` : iso);
const iso = (date) => date.toISOString();

test('isBusinessDay: weekdays, weekends, holidays', () => {
  assert.equal(isBusinessDay(d('2026-07-03')), true);
  assert.equal(isBusinessDay(d('2026-07-04')), false);
  assert.equal(isBusinessDay(d('2026-07-05')), false);
  assert.equal(isBusinessDay(d('2026-07-03'), { holidays: ['2026-07-03'] }), false);
});

test('isBusinessDay uses the UTC day, not local time', () => {
  assert.equal(isBusinessDay(d('2026-07-03T23:59:00Z')), true, 'still Friday in UTC');
  assert.equal(isBusinessDay(d('2026-07-04T00:30:00Z')), false, 'already Saturday in UTC');
});

test('addBusinessDays skips the weekend', () => {
  assert.equal(iso(addBusinessDays(d('2026-07-02'), 1)), '2026-07-03T00:00:00.000Z');
  assert.equal(iso(addBusinessDays(d('2026-07-02'), 2)), '2026-07-06T00:00:00.000Z');
});

test('addBusinessDays skips holidays', () => {
  assert.equal(iso(addBusinessDays(d('2026-07-02'), 1, { holidays: ['2026-07-03'] })), '2026-07-06T00:00:00.000Z');
});

test('addBusinessDays keeps the UTC time of day', () => {
  assert.equal(iso(addBusinessDays(d('2026-07-03T23:30:00Z'), 1)), '2026-07-06T23:30:00.000Z');
});

test('n = 0 rolls a non-business day forward, keeps a business day', () => {
  assert.equal(iso(addBusinessDays(d('2026-07-04T08:00:00Z'), 0)), '2026-07-06T08:00:00.000Z');
  assert.equal(iso(addBusinessDays(d('2026-07-02'), 0)), '2026-07-02T00:00:00.000Z');
});

test('negative n goes backwards', () => {
  assert.equal(iso(addBusinessDays(d('2026-07-06'), -1)), '2026-07-03T00:00:00.000Z');
  assert.equal(iso(addBusinessDays(d('2026-07-06'), -1, { holidays: ['2026-07-03'] })), '2026-07-02T00:00:00.000Z');
});

test('starting on a weekend', () => {
  assert.equal(iso(addBusinessDays(d('2026-07-04'), 1)), '2026-07-06T00:00:00.000Z');
  assert.equal(iso(addBusinessDays(d('2026-07-04'), -1)), '2026-07-03T00:00:00.000Z');
});

test('year boundary with holidays', () => {
  const holidays = ['2026-12-25', '2027-01-01'];
  assert.equal(iso(addBusinessDays(d('2026-12-24'), 6, { holidays })), '2027-01-05T00:00:00.000Z');
  assert.equal(businessDaysBetween(d('2026-12-24'), d('2027-01-05'), { holidays }), 6);
});

test('does not mutate the input date', () => {
  const start = d('2026-07-02');
  addBusinessDays(start, 3);
  assert.equal(iso(start), '2026-07-02T00:00:00.000Z');
});

test('businessDaysBetween counts (start, end] and is antisymmetric', () => {
  assert.equal(businessDaysBetween(d('2026-07-02'), d('2026-07-06')), 2);
  assert.equal(businessDaysBetween(d('2026-07-06'), d('2026-07-02')), -2);
  assert.equal(businessDaysBetween(d('2026-07-02'), d('2026-07-06'), { holidays: ['2026-07-06'] }), 1);
  assert.equal(businessDaysBetween(d('2026-07-04'), d('2026-07-05')), 0);
  assert.equal(businessDaysBetween(d('2026-07-02T09:00:00Z'), d('2026-07-02T17:00:00Z')), 0);
  assert.equal(businessDaysBetween(d('2026-07-02T23:00:00Z'), d('2026-07-03T01:00:00Z')), 1);
});

test('invalid arguments throw RangeError', () => {
  assert.throws(() => addBusinessDays(d('2026-07-02'), 1.5), RangeError);
  assert.throws(() => addBusinessDays(d('2026-07-02'), 1, { holidays: ['3/7/2026'] }), RangeError);
  assert.throws(() => isBusinessDay(d('2026-07-02'), { holidays: ['2026-7-3'] }), RangeError);
});
