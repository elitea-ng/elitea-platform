import { describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK } from '../tokens';
import { avatarInitialsType, labelShrinkScale, typeScale } from '../typeScale';
import { ladderPx, MIN_FONT_PX, sizePx, toTypography } from '../typography';

/**
 * The one type scale (typography spec rev. 2): four sizes, eight variants,
 * every MUI stock variant aliased onto them.
 *
 * N4 parity with `MainTheme.js:17-89` is DELIBERATELY DROPPED here for three
 * things, and a change back has to be argued, not absorbed:
 *  - line heights: unitless ratios now (the baseline's fixed rem leading was
 *    inherited by larger text and ignored WCAG 1.4.12 overrides);
 *  - `labelTiny` (10px): removed, it sat under the 12px floor;
 *  - `bodySmall2`: removed, a second 12px body that differed only in leading.
 * The SIZES at the default pack are unchanged: 12 / 14 / 16 / 20.
 *
 * The expectations below are named `size`/`leading` rather than
 * `fontSize`/`lineHeight` so the tables stay DATA: a property literally
 * called `fontSize:` with a literal value is exactly what R-T11
 * (`elitea/ad-hoc-font-size`) forbids, and this fixture must not need an
 * exemption from the fence it verifies.
 */

type Built = Record<string, Record<string, unknown>>;

const CUSTOM = [
  'headingLarge',
  'headingMedium',
  'headingSmall',
  'labelMedium',
  'bodyMedium',
  'labelSmall',
  'bodySmall',
  'subtitle',
] as const;

const ALIASES = {
  body1: 'bodyMedium',
  body2: 'bodyMedium',
  caption: 'bodySmall',
  subtitle1: 'headingSmall',
  subtitle2: 'labelMedium',
  h1: 'headingLarge',
  h2: 'headingLarge',
  h3: 'headingLarge',
  h4: 'headingLarge',
  h5: 'headingMedium',
  h6: 'headingMedium',
  overline: 'subtitle',
  button: 'labelSmall',
} as const;

const DEFAULT = DEFAULT_BRAND_PACK.typography;

const PACKS = [
  { label: 'default (14 × 1.2)', baseSize: DEFAULT.baseSize, scale: DEFAULT.scale, ladder: [12, 14, 16, 20] },
  { label: '12 × 1.05', baseSize: 12, scale: 1.05, ladder: [12, 14, 16, 18] },
  { label: '12 × 1.5', baseSize: 12, scale: 1.5, ladder: [12, 14, 18, 28] },
  // Raw rungs 18/18/18/20: the upward walk lifts 0, +1 and +2 off the −1 rung.
  { label: '18 × 1.05', baseSize: 18, scale: 1.05, ladder: [18, 20, 22, 24] },
  { label: '18 × 1.5', baseSize: 18, scale: 1.5, ladder: [12, 18, 28, 40] },
] as const;

const build = (baseSize: number, scale: number, prefix?: string): Built =>
  toTypography(
    { fontFamily: DEFAULT.fontFamily, fontFamilyMono: DEFAULT.fontFamilyMono, baseSize, scale },
    prefix,
  ) as Built;

const px = (value: unknown): number => Number.parseFloat(String(value)) * 16;

describe('the raw modular rung', () => {
  it('rounds to even pixels, which is what makes it hit 12/14/16/20 at the default pack', () => {
    expect([2, 1, 0, -1].map((step) => sizePx(step, 14, 1.2))).toEqual([20, 16, 14, 12]);
    // The naive round would put step +1 at 17px.
    expect(Math.round(14 * 1.2)).toBe(17);
  });
});

describe.each(PACKS)('pack $label', ({ baseSize, scale, ladder }) => {
  const built = build(baseSize, scale);
  const all = [...CUSTOM, ...Object.keys(ALIASES)];

  it('(a) builds the expected ladder', () => {
    const rungs = ladderPx(baseSize, scale);
    expect([rungs[-1], rungs[0], rungs[1], rungs[2]]).toEqual(ladder);
    expect(px(built['bodySmall']?.['fontSize'])).toBe(ladder[0]);
    expect(px(built['bodyMedium']?.['fontSize'])).toBe(ladder[1]);
    expect(px(built['headingMedium']?.['fontSize'])).toBe(ladder[2]);
    expect(px(built['headingLarge']?.['fontSize'])).toBe(ladder[3]);
  });

  it('(b) renders at most four distinct sizes, stock aliases included', () => {
    const sizes = new Set(all.map((name) => built[name]?.['fontSize']));
    expect(sizes.size).toBeLessThanOrEqual(4);
  });

  it('(c) never renders under the floor', () => {
    for (const name of all) expect(px(built[name]?.['fontSize'])).toBeGreaterThanOrEqual(MIN_FONT_PX);
  });

  it('(d) has strictly increasing rungs', () => {
    for (let i = 1; i < ladder.length; i++) expect(ladder[i]).toBeGreaterThan(ladder[i - 1] as number);
  });

  it('(e) uses unitless leading: body ≥ 1.43, every variant ≥ 1.4', () => {
    for (const name of all) {
      const leading = built[name]?.['lineHeight'];
      expect(typeof leading, name).toBe('number');
      expect(leading as number, name).toBeGreaterThanOrEqual(1.4);
    }
    for (const name of ['bodyMedium', 'bodySmall', 'body1', 'body2']) {
      expect(built[name]?.['lineHeight'] as number).toBeGreaterThanOrEqual(1.43);
    }
  });

  it('(f) aliases every stock variant to typeScale(target), without colour', () => {
    for (const [stock, target] of Object.entries(ALIASES)) {
      expect(built[stock], stock).toEqual(typeScale(built[target] as Record<string, never>));
      expect(built[stock], stock).not.toHaveProperty('color');
    }
  });

  it('(g) shrinks a labelMedium floating label exactly onto rung −1', () => {
    const k = labelShrinkScale(built as never);
    expect(Math.round(px(built['labelMedium']?.['fontSize']) * k)).toBe(ladder[0]);
  });
});

describe('default-pack variants', () => {
  const built = build(DEFAULT.baseSize, DEFAULT.scale);
  const SPEC = {
    headingLarge: { fontWeight: 600, size: '1.25rem', leading: 1.4 },
    headingMedium: { fontWeight: 600, size: '1rem', leading: 1.5 },
    headingSmall: { fontWeight: 600, size: '0.875rem', leading: 1.43 },
    labelMedium: { fontWeight: 500, size: '0.875rem', leading: 1.43 },
    bodyMedium: { fontWeight: 400, size: '0.875rem', leading: 1.43 },
    labelSmall: { fontWeight: 500, size: '0.75rem', leading: 1.4 },
    bodySmall: { fontWeight: 400, size: '0.75rem', leading: 1.5 },
    subtitle: { fontWeight: 500, size: '0.75rem', leading: 1.4 },
  } as const;

  it.each(Object.entries(SPEC))('%s matches the spec', (name, expected) => {
    const variant = built[name];
    expect(variant?.['fontSize']).toBe(expected.size);
    expect(variant?.['lineHeight']).toBe(expected.leading);
    expect(variant?.['fontWeight']).toBe(expected.fontWeight);
    expect(variant?.['fontStyle']).toBe('normal');
  });

  it('keeps the subtitle uppercase, with relative tracking', () => {
    expect(built['subtitle']).toMatchObject({ letterSpacing: '0.06em', textTransform: 'uppercase' });
  });

  it('removes labelTiny, bodySmall2 and the dead labelLarge', () => {
    for (const name of ['labelTiny', 'bodySmall2', 'labelLarge']) expect(built).not.toHaveProperty(name);
  });

  it('paints the three heading variants with the text.secondary token', () => {
    for (const name of ['headingLarge', 'headingMedium', 'headingSmall']) {
      expect(built[name]?.['color']).toBe('var(--el-palette-text-secondary)');
    }
    expect(built['bodyMedium']).not.toHaveProperty('color');
  });

  it('names that token under the prefix of the theme it is built into', () => {
    // A theme built under another scope (the Branding preview) must not name
    // the APP theme's variable, or its headings take the outer scheme's
    // colour: white, from the dark console, on the preview's light surface.
    const scoped = build(DEFAULT.baseSize, DEFAULT.scale, 'elp');
    for (const name of ['headingLarge', 'headingMedium', 'headingSmall']) {
      expect(scoped[name]?.['color']).toBe('var(--elp-palette-text-secondary)');
    }
    expect(JSON.stringify(scoped)).not.toContain('--el-');
  });

  it('carries the pack’s families, base size and the baseline feature settings', () => {
    expect(built['fontFamily']).toBe(DEFAULT.fontFamily);
    expect(built['fontFamilyMono']).toBe(DEFAULT.fontFamilyMono);
    expect(built['fontSize']).toBe(14);
    expect(built['fontFeatureSettings']).toBe('"clig" 0, "liga" 0');
  });
});

describe('typeScale', () => {
  it('copies size, weight, leading and tracking, and never colour', () => {
    const built = build(DEFAULT.baseSize, DEFAULT.scale);
    const copy = typeScale(built['headingLarge'] as Record<string, never>);
    expect(Object.keys(copy).sort()).toEqual(['fontSize', 'fontWeight', 'letterSpacing', 'lineHeight']);
    expect(copy).not.toHaveProperty('color');
  });
});

describe('avatarInitialsType', () => {
  it('snaps initials to the ladder by avatar size', () => {
    expect([16, 24, 28, 32, 40, 48, 64].map(avatarInitialsType)).toEqual([
      'labelSmall',
      'labelSmall',
      'labelMedium',
      'labelMedium',
      'headingMedium',
      'headingMedium',
      'headingLarge',
    ]);
  });
});
