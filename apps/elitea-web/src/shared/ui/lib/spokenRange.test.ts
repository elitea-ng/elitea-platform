import { marked, type MarkedToken } from 'marked';
import { describe, expect, it } from 'vitest';

import { isPlainTextToken, spokenOverlapWithin, splitForHighlight, stampTokenPositions } from './spokenRange';

describe('stampTokenPositions', () => {
  it('stamps each top-level token with its cumulative offset into the source', () => {
    const source = 'First paragraph.\n\nSecond paragraph.';
    const tokens = marked.lexer(source) as MarkedToken[];
    const stamped = stampTokenPositions(tokens);

    expect(stamped).toHaveLength(tokens.length);
    // The stamped span, sliced back out of the ORIGINAL source, reproduces
    // exactly the token's own `raw` — the property the highlight depends on.
    for (const { token, startPos, endPos } of stamped) {
      expect(source.slice(startPos, endPos)).toBe(token.raw);
    }
  });

  it('is empty for no tokens', () => {
    expect(stampTokenPositions([])).toEqual([]);
  });
});

describe('spokenOverlapWithin', () => {
  it('is undefined while TTS is idle (no spokenRange)', () => {
    expect(spokenOverlapWithin(undefined, 0, 10)).toBeUndefined();
  });

  it('is undefined when the range falls entirely before the token', () => {
    expect(spokenOverlapWithin({ start: 0, end: 5 }, 10, 20)).toBeUndefined();
  });

  it('is undefined when the range falls entirely after the token', () => {
    expect(spokenOverlapWithin({ start: 40, end: 50 }, 10, 20)).toBeUndefined();
  });

  it('clips the overlap to the token span, as offsets INTO the token', () => {
    // Token spans source[10, 30). The spoken range [5, 20) starts before the
    // token and ends inside it — the overlap is [0, 10) in TOKEN-relative terms.
    expect(spokenOverlapWithin({ start: 5, end: 20 }, 10, 20)).toEqual({ start: 0, end: 10 });
  });

  it('is undefined at an exact touching boundary (no actual overlap)', () => {
    expect(spokenOverlapWithin({ start: 20, end: 30 }, 10, 10)).toBeUndefined();
  });
});

describe('isPlainTextToken', () => {
  it('accepts a paragraph with no inline formatting', () => {
    const [token] = marked.lexer('Hello world.') as MarkedToken[];
    expect(token?.type).toBe('paragraph');
    expect(isPlainTextToken(token as MarkedToken)).toBe(true);
  });

  it('refuses a paragraph that carries inline formatting (a bold span)', () => {
    const [token] = marked.lexer('Hello **world**.') as MarkedToken[];
    expect(token?.type).toBe('paragraph');
    expect(isPlainTextToken(token as MarkedToken)).toBe(false);
  });

  it('refuses a non-text/paragraph token (a heading)', () => {
    const [token] = marked.lexer('# Title') as MarkedToken[];
    expect(token?.type).toBe('heading');
    expect(isPlainTextToken(token as MarkedToken)).toBe(false);
  });
});

describe('splitForHighlight', () => {
  it('slices before/highlighted/after out of raw text', () => {
    expect(splitForHighlight('the quick brown fox', { start: 4, end: 9 })).toEqual({
      before: 'the ',
      highlighted: 'quick',
      after: ' brown fox',
    });
  });

  it('is a no-op split (empty highlighted) for a zero-width overlap', () => {
    expect(splitForHighlight('hello', { start: 2, end: 2 })).toEqual({
      before: 'he',
      highlighted: '',
      after: 'llo',
    });
  });
});
