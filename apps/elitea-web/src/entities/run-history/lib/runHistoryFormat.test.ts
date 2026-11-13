import { describe, expect, it } from 'vitest';

import { formatRunTimestamp } from './formatRunTimestamp';
import { exchangeOf, runPreview } from './runExchange';

function group(role: string, text: string, uuid = `${role}-${text}`): Record<string, unknown> {
  return { uuid, role, message_items: [{ item_type: 'text_message', item_details: { content: text } }] };
}

describe('formatRunTimestamp', () => {
  it('reads the server wall clock as UTC and shows it in the reader local time, not as raw ISO', () => {
    const shown = formatRunTimestamp('2026-11-12T16:30:11.816831');
    expect(shown).not.toContain('T16:30');
    expect(shown).not.toContain('816831');
    const expected = new Date('2026-11-12T16:30:11.816Z').toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
    expect(shown).toBe(expected);
  });

  it('keeps a zone the value already carries', () => {
    const expected = new Date('2026-11-12T16:30:11Z').toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
    expect(formatRunTimestamp('2026-11-12T16:30:11+00:00')).toBe(expected);
    expect(formatRunTimestamp('2026-11-12T16:30:11Z')).toBe(expected);
  });

  it('returns text that is not a date unchanged, and a dash for none', () => {
    expect(formatRunTimestamp('yesterday-ish')).toBe('yesterday-ish');
    expect(formatRunTimestamp(undefined)).toBe('—');
    expect(formatRunTimestamp('')).toBe('—');
  });
});

describe('runPreview', () => {
  it('is the first user message, not the model answer that may come first in the list', () => {
    expect(runPreview([group('assistant', 'Hi'), group('user', 'Summarise the report')], 'text generator')).toBe('Summarise the report');
  });

  it('falls back to the title with no user message or no data yet', () => {
    expect(runPreview([group('assistant', 'Only an answer')], 'text generator')).toBe('text generator');
    expect(runPreview(undefined, 'text generator')).toBe('text generator');
    expect(runPreview([], 'text generator')).toBe('text generator');
  });

  it('is one bounded line', () => {
    const preview = runPreview([group('user', `line one\n\nline   two ${'x'.repeat(300)}`)], 'title');
    expect(preview).not.toContain('\n');
    expect(preview.startsWith('line one line two')).toBe(true);
    expect(preview.length).toBeLessThanOrEqual(120);
    expect(preview.endsWith('…')).toBe(true);
  });
});

describe('exchangeOf', () => {
  it('keeps both sides in order and drops messages with no text', () => {
    const entries = exchangeOf([
      group('user', 'Question'),
      { uuid: 'x', role: 'assistant', message_items: [{ item_type: 'attachment_message' }] },
      group('assistant', 'Answer'),
    ]);
    expect(entries.map((entry) => [entry.role, entry.text])).toEqual([
      ['user', 'Question'],
      ['assistant', 'Answer'],
    ]);
  });
});
