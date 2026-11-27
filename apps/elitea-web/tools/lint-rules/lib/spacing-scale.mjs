/**
 * The spacing scale for R-T9 as amended by ADR-0062 decision 2.
 *
 * The scale is the brand's `space.steps` (px) divided by the web theme's
 * spacing unit: 8 px, the comfortable unit at the 16 px browser default
 * (`src/shared/brand/buildTheme.ts` `spacing:` writes 8, or 0.5rem after
 * #3093), not the pack's `space.unit`, which is 4. The steps are read
 * from `tokens.json` so the scale is never copied by hand.
 */
import { readFileSync } from 'node:fs';

const TOKENS_URL = new URL('../../../../../libs/ts/client-core/src/brand/tokens.json', import.meta.url);

/** Root font size a rem length is converted with (the browser default). */
export const REM_PX = 16;

/** Brand steps in px, from `tokens.json` `space.steps`. */
function brandSteps() {
  return JSON.parse(readFileSync(TOKENS_URL, 'utf8')).space.steps;
}

/** The scale in theme units: each brand step divided by `unit`. */
export function scaleFor(unit, steps = brandSteps()) {
  return steps.map((step) => step / unit);
}

/** True when `n` (a multiple of the unit) is a step; negatives are margins. */
export function onScale(n, scale) {
  return scale.includes(Math.abs(n));
}

/** The steps just under and just over |n|, sign kept: `0.75` → `[0.5, 1]`. */
export function nearestSteps(n, scale) {
  const abs = Math.abs(n);
  const sign = n < 0 ? -1 : 1;
  const sorted = [...scale].sort((a, b) => a - b);
  const below = sorted.filter((s) => s < abs).pop();
  const above = sorted.find((s) => s > abs);
  return [below, above].filter((s) => s !== undefined).map((s) => sign * s);
}

/** Rounds away float noise from px ÷ unit (`12 / 8` stays `1.5`, `4.8 / 8` → `0.6`). */
export function toUnits(px, unit) {
  return Math.round((px / unit) * 1000) / 1000;
}
