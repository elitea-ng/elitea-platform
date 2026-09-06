import { describe, expect, it } from 'vitest';

import { format } from 'date-fns';

import { formatNotificationTimestamp, normalizeNotificationTimestamp } from './timestamp';

describe('normalizeNotificationTimestamp', () => {
  it('joins a space-separated naive timestamp with T...Z (convertChatConversationMessages.js:22-24)', () => {
    expect(normalizeNotificationTimestamp('2026-01-01 12:00:00')).toBe('2026-01-01T12:00:00Z');
  });

  it('leaves a string already ending in Z unchanged', () => {
    expect(normalizeNotificationTimestamp('2026-01-01T12:00:00Z')).toBe('2026-01-01T12:00:00Z');
  });

  it('leaves a string carrying an explicit +offset unchanged', () => {
    expect(normalizeNotificationTimestamp('2026-01-01T12:00:00+02:00')).toBe('2026-01-01T12:00:00+02:00');
  });

  it('appends Z to a bare ISO string with no offset and no space', () => {
    expect(normalizeNotificationTimestamp('2026-01-01T12:00:00')).toBe('2026-01-01T12:00:00Z');
  });

  it('produces a Date-parseable UTC string', () => {
    const normalized = normalizeNotificationTimestamp('2026-01-01 12:00:00');
    expect(new Date(normalized).toISOString()).toBe('2026-01-01T12:00:00.000Z');
  });
});

describe('formatNotificationTimestamp', () => {
  it("renders the reference's `dd-MMM-yyyy, kk:mm`", () => {
    // The live page shows `12-Jun-2026, 15:55` for its one row.
    expect(formatNotificationTimestamp('2026-06-12T15:55:00Z')).toBe(
      format(new Date('2026-06-12T15:55:00Z'), 'dd-MMM-yyyy, kk:mm'),
    );
  });

  it('reads a zone-less backend timestamp as UTC, not as local time', () => {
    // The single-item notification endpoints return `2006-01-02T15:04:05`
    // with no zone. Without the normalise step the column shifts by the
    // viewer's offset, silently, and only for rows from those endpoints.
    expect(formatNotificationTimestamp('2026-06-12 15:55:00')).toBe(
      formatNotificationTimestamp('2026-06-12T15:55:00Z'),
    );
  });

  it('renders an empty cell rather than the words "Invalid Date"', () => {
    expect(formatNotificationTimestamp(undefined)).toBe('');
    expect(formatNotificationTimestamp('')).toBe('');
    expect(formatNotificationTimestamp('not a date')).toBe('');
  });
});
