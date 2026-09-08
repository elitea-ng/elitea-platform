/**
 * Ported byte-for-byte from `EliteaUI/src/assets/context.svg` — the 40x40
 * glyph above Settings › Project Context's empty state.
 *
 * Its `#686C76` fill is kept rather than rewritten to `currentColor`, which
 * is this set's usual convention (196 of 200 icons). The reference ships the
 * literal fill and a live deployment renders it: the path computes to
 * `rgb(104, 108, 118)` on the real page. Making it inherit would have drawn
 * it in the empty state's own text colour, `rgb(169, 183, 193)`, which is a
 * visibly lighter grey.
 */
export { default as ContextIcon } from './svg/context-icon.svg?react';
