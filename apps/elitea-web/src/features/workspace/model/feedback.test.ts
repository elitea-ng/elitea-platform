import { describe, expect, it } from 'vitest';

import { MAX_FEEDBACK_BYTES, clipUtf8 } from './feedback';

const bytes = (text: string): number => new TextEncoder().encode(text).length;

describe('clipUtf8', () => {
  it('keeps text within the bound as it is', () => {
    expect(clipUtf8('short')).toBe('short');
    expect(clipUtf8('a'.repeat(MAX_FEEDBACK_BYTES))).toHaveLength(MAX_FEEDBACK_BYTES);
  });

  it('bounds by UTF-8 bytes, not UTF-16 units, and never splits a character', () => {
    // "é" is one UTF-16 unit and two bytes: 2048 of them are 4 KiB.
    const clipped = clipUtf8('é'.repeat(MAX_FEEDBACK_BYTES));
    expect(bytes(clipped)).toBe(MAX_FEEDBACK_BYTES);
    expect(clipped).toBe('é'.repeat(MAX_FEEDBACK_BYTES / 2));
    // An emoji is two units and four bytes: three bytes keep none of it.
    expect(clipUtf8('ab😀', 5)).toBe('ab');
    expect(clipUtf8('ab😀', 6)).toBe('ab😀');
  });
});
