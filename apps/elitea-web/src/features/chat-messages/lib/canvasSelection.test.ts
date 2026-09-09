/**
 * The range contract, stated over text the byte/character difference is
 * visible in.
 *
 * An ASCII-only suite passes for a client that sends character offsets, which
 * is exactly the defect this module exists to stop: the server slices a Go
 * string by BYTE, so every non-ASCII character before the selection shifts the
 * carve left by the bytes it costs, and the route accepts it.
 */
import { describe, expect, it } from 'vitest';

import { canvasByteRange, canvasKindForSelection, selectionTextWithin, utf8ByteLength } from './canvasSelection';

describe('utf8ByteLength', () => {
  it('counts bytes, not JavaScript characters', () => {
    expect(utf8ByteLength('abc')).toBe(3);
    // 2 bytes each, one JS character each.
    expect(utf8ByteLength('éé')).toBe(4);
    // 3 bytes, one JS character.
    expect(utf8ByteLength('日')).toBe(3);
    // 4 bytes, TWO JS characters (a surrogate pair).
    expect('🌍'.length).toBe(2);
    expect(utf8ByteLength('🌍')).toBe(4);
  });
});

describe('canvasByteRange', () => {
  it('answers the byte offsets of an ASCII selection', () => {
    const stored = 'hello world';
    expect(canvasByteRange(stored, 'world')).toEqual({ startsAt: 6, endsAt: 11 });
  });

  it('shifts the start past every byte of the multibyte text before it', () => {
    // 'héllo ' is 6 characters and 7 bytes; 'wörld' is 5 characters, 6 bytes.
    const stored = 'héllo wörld';
    expect(stored.indexOf('wörld'), 'the character index a naive client would send').toBe(6);
    expect(canvasByteRange(stored, 'wörld')).toEqual({ startsAt: 7, endsAt: 13 });
  });

  it('counts a surrogate pair as four bytes, not two characters', () => {
    const stored = '🌍 then code';
    expect(stored.indexOf('code')).toBe(8);
    // 4 bytes for the emoji + ' then ' (6) = 10.
    expect(canvasByteRange(stored, 'code')).toEqual({ startsAt: 10, endsAt: 14 });
  });

  it('measures a selection that is itself multibyte', () => {
    const stored = 'prefix 日本語 suffix';
    const range = canvasByteRange(stored, '日本語');
    expect(range).toEqual({ startsAt: 7, endsAt: 16 });
    // The proof the range is the one the SERVER would slice: re-slice the
    // encoded bytes with it and decode what comes back.
    const bytes = new TextEncoder().encode(stored);
    expect(new TextDecoder().decode(bytes.slice(range?.startsAt, range?.endsAt))).toBe('日本語');
  });

  it('refuses a selection the stored text does not contain, rather than carving the nearest one', () => {
    expect(canvasByteRange('the stored answer', 'words rendered from markup')).toBeUndefined();
  });

  it('refuses an empty or whitespace-only selection', () => {
    expect(canvasByteRange('some text', '')).toBeUndefined();
    expect(canvasByteRange('some text', '   \n ')).toBeUndefined();
    expect(canvasByteRange('', 'text')).toBeUndefined();
  });

  it('trims the selection: a drag that swept up the trailing newline still carves the words', () => {
    const stored = 'alpha\nbeta\ngamma';
    expect(canvasByteRange(stored, 'beta\n')).toEqual({ startsAt: 6, endsAt: 10 });
  });
});

describe('canvasKindForSelection (issue #879)', () => {
  it('reads a selection that is a single fenced block, start to end, as code', () => {
    expect(canvasKindForSelection('```js\nconsole.log(1);\n```')).toBe('code');
    // Leading/trailing whitespace from the drag is trimmed before judging the fence.
    expect(canvasKindForSelection('  \n```python\nprint(1)\n```\n  ')).toBe('code');
  });

  it('reads a paragraph, a heading, or an inline snippet as document (prose)', () => {
    expect(canvasKindForSelection('Just a paragraph of plain prose.')).toBe('document');
    expect(canvasKindForSelection('# A heading\n\nSome body text.')).toBe('document');
    // Mentions a snippet inline but is not ITSELF a fence start-to-end.
    expect(canvasKindForSelection('Run `npm test` to check it.')).toBe('document');
    expect(canvasKindForSelection('Before the fence ```code``` after it')).toBe('document');
  });
});

describe('selectionTextWithin', () => {
  /** A minimal stand-in for the two `Selection` members the rule reads. */
  function fakeSelection(options: { collapsed?: boolean; ranges?: number; ancestor?: Node; text?: string }): Selection {
    return {
      isCollapsed: options.collapsed ?? false,
      rangeCount: options.ranges ?? 1,
      getRangeAt: () => ({ commonAncestorContainer: options.ancestor as Node }) as Range,
      toString: () => options.text ?? '',
    } as unknown as Selection;
  }

  it('answers the selected text when the range sits inside the container', () => {
    const container = document.createElement('div');
    const inner = document.createElement('span');
    container.appendChild(inner);
    expect(selectionTextWithin(container, fakeSelection({ ancestor: inner, text: 'picked' }))).toBe('picked');
  });

  it('answers nothing for a selection in a DIFFERENT message', () => {
    const container = document.createElement('div');
    const elsewhere = document.createElement('div');
    expect(selectionTextWithin(container, fakeSelection({ ancestor: elsewhere, text: 'picked' }))).toBeUndefined();
  });

  it('answers nothing for a caret, an empty range list, whitespace, or no container', () => {
    const container = document.createElement('div');
    expect(selectionTextWithin(container, fakeSelection({ collapsed: true, ancestor: container, text: 'x' }))).toBeUndefined();
    expect(selectionTextWithin(container, fakeSelection({ ranges: 0, ancestor: container, text: 'x' }))).toBeUndefined();
    expect(selectionTextWithin(container, fakeSelection({ ancestor: container, text: '  ' }))).toBeUndefined();
    expect(selectionTextWithin(null, fakeSelection({ ancestor: container, text: 'x' }))).toBeUndefined();
    expect(selectionTextWithin(container, null)).toBeUndefined();
  });

  /*
   * A MEASURED browser state, not an invented one. A paragraph is a block as
   * wide as the answer bubble; a double click in the empty space PAST the end
   * of a short line leaves chromium reporting a range that is NOT collapsed
   * and whose text is the empty string. `isCollapsed` alone therefore does
   * not answer "did the reader highlight anything" — the emptiness of the
   * text does, and a rule that trusted the flag would offer to carve a range
   * of nothing.
   */
  it('answers nothing for the empty range a click past the end of a line leaves', () => {
    const container = document.createElement('div');
    expect(selectionTextWithin(container, fakeSelection({ collapsed: false, ancestor: container, text: '' }))).toBeUndefined();
  });
});
