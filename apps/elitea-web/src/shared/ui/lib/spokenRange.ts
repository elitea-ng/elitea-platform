import type { MarkedToken } from 'marked';

/**
 * The word currently being read aloud, as an offset RANGE into the markdown
 * source string `Markdown` was given — the same coordinate space the
 * browser's `SpeechSynthesisUtterance` `boundary` event reports (issue #625
 * item 1; ported, reduced, from `apps/elitea-ui/src/[fsd]/shared/ui/
 * markdown/{Markdown,Token}.jsx`'s `spokenRange`/`startPos`/`endPos`).
 *
 * `undefined` (the prop's default) means "TTS is idle" — nothing highlights.
 */
export interface SpokenRange {
  readonly start: number;
  readonly end: number;
}

/** One top-level token, plus its `[startPos, endPos)` offset into the original source. */
export interface StampedToken {
  readonly token: MarkedToken;
  readonly startPos: number;
  readonly endPos: number;
}

/**
 * Stamps every top-level token with its offset into the source `marked.lexer`
 * split it from, by walking `raw.length` cumulatively. Positions are only
 * stamped at this top level — `Token` does not highlight inside a nested
 * container (list item, blockquote, table cell); see `isPlainTextToken`.
 */
export function stampTokenPositions(tokens: readonly MarkedToken[]): readonly StampedToken[] {
  let pos = 0;
  return tokens.map((token) => {
    const startPos = pos;
    pos += token.raw.length;
    return { token, startPos, endPos: pos };
  });
}

/**
 * The part of `spokenRange` that falls inside one token's span, expressed as
 * offsets into the TOKEN's own `raw` text (0-based) — or `undefined` when
 * `spokenRange` is absent (TTS idle) or does not reach this token at all.
 */
export function spokenOverlapWithin(
  spokenRange: SpokenRange | undefined,
  startPos: number,
  rawLength: number,
): SpokenRange | undefined {
  if (!spokenRange) return undefined;
  const overlapStart = Math.max(spokenRange.start, startPos) - startPos;
  const overlapEnd = Math.min(spokenRange.end, startPos + rawLength) - startPos;
  return overlapStart < overlapEnd ? { start: overlapStart, end: overlapEnd } : undefined;
}

type PlainTextToken = Extract<MarkedToken, { type: 'paragraph' | 'text' }>;

/**
 * Whether a token's inline content is plain text with no nested formatting
 * (bold/link/code/…) — the one case the highlight can safely slice out of
 * `raw` without splitting a tag in half. Matches the reference `Token.jsx`
 * 'text' case guard (`tokens?.some(t => t.type !== 'text')`), generalized to
 * 'paragraph' too since this port's 'text' case is the tight-list-item
 * wrapper, not the answer body's own paragraphs.
 *
 * A token that fails this check still renders — through the normal
 * `DefaultMarkdown` path — it just does not highlight, which is the safe
 * default (correct content, no highlight) rather than a corrupted span.
 */
export function isPlainTextToken(token: MarkedToken): token is PlainTextToken {
  if (token.type !== 'paragraph' && token.type !== 'text') return false;
  const children = token.tokens;
  return children === undefined || children.every((child) => child.type === 'text');
}

/** `raw`, split into the part before, inside, and after one highlight range. */
export interface HighlightSplit {
  readonly before: string;
  readonly highlighted: string;
  readonly after: string;
}

/** Pure string slicing — no DOM, no sanitizing, so a plain-text span can never corrupt markup. */
export function splitForHighlight(raw: string, overlap: SpokenRange): HighlightSplit {
  return {
    before: raw.slice(0, overlap.start),
    highlighted: raw.slice(overlap.start, overlap.end),
    after: raw.slice(overlap.end),
  };
}
