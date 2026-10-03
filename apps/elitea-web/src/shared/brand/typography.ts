import type { TypographyVariantsOptions } from '@mui/material/styles';
import type { CSSProperties } from 'react';

import { CSS_VAR_PREFIX } from './constants';
import type { BrandPack } from './schema';

/**
 * Typography (spec §4.2 tier 2; typography spec rev. 2). ONE type scale for
 * the app and the admin console: four sizes, eight variants.
 *
 * The ladder: `sizePx(step) = 2 * round(baseSize * scale ** step / 2)`,
 * then two guards (see `ladderPx`):
 *
 *  - floor: no rung under 12px (WCAG-minded metadata floor);
 *  - upward separation: walking from step −1 upward, every rung sits at
 *    least 2px above the one below. A rung is only ever pushed UP, so the
 *    floor and the separation cannot fight.
 *
 * At the default pack (14, 1.2):
 *
 *   step  raw px   ladder px   used by
 *    +2   20.16      20        headingLarge
 *    +1   16.80      16        headingMedium
 *     0   14.00      14        headingSmall / labelMedium / bodyMedium
 *    -1   11.67      12        labelSmall / bodySmall / subtitle
 *
 * Every MUI stock variant (body1, body2, caption, h1–h6, subtitle1/2,
 * overline, button) is ALIASED onto one of the eight below through
 * `typeScale`, so a component that renders a stock variant internally
 * (DialogTitle → h6, CssBaseline's `<body>` → body1, DataGrid → body2)
 * lands on the same four sizes instead of MUI's own 11/13/15/16/20/24.
 *
 * Line heights are UNITLESS ratios: inherited as a ratio, they keep larger
 * text inside a body1-styled parent from inheriting a fixed 20px leading,
 * and they follow WCAG 1.4.12 overrides and text-only zoom. Letter spacing
 * is relative (em) for the same reason.
 *
 * N4 parity with `MainTheme.js:17-89` is deliberately dropped for the line
 * heights (were fixed rem), for `labelTiny` (10px, under the floor) and for
 * `bodySmall2` (a second 12px body with a different leading).
 *
 * Weight, style and text-transform are design-system constants: the §4.2
 * pack schema has no field for them, and adding a REQUIRED field would break
 * the Go mirror (unit W3).
 */
interface VariantSpec {
  /** Rung on the modular scale. */
  step: Rung;
  /** Unitless line-height ratio. */
  lineHeight: number;
  fontWeight: number;
  /** Relative tracking; `normal` unless stated. */
  letterSpacing?: string;
  textTransform?: 'uppercase';
  /** Variants the baseline paints with `theme.palette.text.secondary`. */
  secondaryText?: true;
}

/** The four rungs this scale uses. */
type Rung = -1 | 0 | 1 | 2;
const RUNGS: readonly Rung[] = [-1, 0, 1, 2];

const VARIANTS = {
  headingLarge: { step: 2, lineHeight: 1.4, fontWeight: 600, secondaryText: true },
  headingMedium: { step: 1, lineHeight: 1.5, fontWeight: 600, secondaryText: true },
  headingSmall: { step: 0, lineHeight: 1.43, fontWeight: 600, secondaryText: true },
  labelMedium: { step: 0, lineHeight: 1.43, fontWeight: 500 },
  bodyMedium: { step: 0, lineHeight: 1.43, fontWeight: 400 },
  labelSmall: { step: -1, lineHeight: 1.4, fontWeight: 500 },
  bodySmall: { step: -1, lineHeight: 1.5, fontWeight: 400 },
  subtitle: {
    step: -1,
    lineHeight: 1.4,
    fontWeight: 500,
    letterSpacing: '0.06em',
    textTransform: 'uppercase',
  },
} as const satisfies Record<string, VariantSpec>;

/** @public Wave-1 surface: unit S1 types `<Typography variant>` call sites with it. */
export type EliteaTypographyVariant = keyof typeof VARIANTS;

/**
 * Every MUI stock variant → the custom variant it renders as. Only the
 * scale (`typeScale`) is copied — never the heading colour.
 */
const STOCK_ALIASES = {
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
} as const satisfies Record<string, EliteaTypographyVariant>;

/** @public Read by the theme unit test and by R-T13's documentation. */
export type StockTypographyVariant = keyof typeof STOCK_ALIASES;

/** The smallest size any rung may render at. */
export const MIN_FONT_PX = 12;
/** The minimum gap between two neighbouring rungs. */
const RUNG_GAP_PX = 2;

/** The html root size rem values are authored against. */
const ROOT_FONT_SIZE = 16;

/** `2 * round(base * scale ** step / 2)` — the raw, unguarded rung. */
export function sizePx(step: number, baseSize: number, scale: number): number {
  return 2 * Math.round((baseSize * scale ** step) / 2);
}

/**
 * The guarded ladder: rung → px, floored at {@link MIN_FONT_PX} and walked
 * upward so each rung clears the one below by {@link RUNG_GAP_PX}.
 */
export function ladderPx(baseSize: number, scale: number): Record<Rung, number> {
  const out = {} as Record<Rung, number>;
  let previous = -Infinity;
  for (const step of RUNGS) {
    const floored = Math.max(MIN_FONT_PX, sizePx(step, baseSize, scale));
    const separated = Math.max(floored, previous + RUNG_GAP_PX);
    out[step] = separated;
    previous = separated;
  }
  return out;
}

/** Rounded to 4 decimals: enough for sub-pixel fidelity, short enough to read. */
const round4 = (value: number): number => Number(value.toFixed(4));

const rem = (px: number): string => `${round4(px / ROOT_FONT_SIZE)}rem`;

/** The scale half of a typography variant — what an alias or override copies. */
export interface TypeScale {
  fontSize: CSSProperties['fontSize'];
  fontWeight: CSSProperties['fontWeight'];
  lineHeight: CSSProperties['lineHeight'];
  letterSpacing: CSSProperties['letterSpacing'];
}

/**
 * The ONE copy rule: every alias and every component override spreads
 * `typeScale(theme.typography.X)`, never the variant itself. It returns the
 * size, weight, leading and tracking — and never `color`, so a heading
 * variant's `text.secondary` does not leak into a menu item, a table cell or
 * a dialog title (the Typography-`color` trap, in override form).
 */
export function typeScale(variant: CSSProperties): TypeScale {
  return {
    fontSize: variant.fontSize,
    fontWeight: variant.fontWeight,
    lineHeight: variant.lineHeight,
    letterSpacing: variant.letterSpacing ?? 'normal',
  };
}

/**
 * The factor that maps a step-0 label onto the step −1 rung — what a shrunk
 * floating label (and its outline notch) scale by. Computed from the BUILT
 * ladder, never a constant, so it stays a ladder rung for every pack.
 */
export function labelShrinkScale(typography: {
  bodySmall: CSSProperties;
  bodyMedium: CSSProperties;
}): number {
  const small = Number.parseFloat(String(typography.bodySmall.fontSize));
  const medium = Number.parseFloat(String(typography.bodyMedium.fontSize));
  return round4(small / medium);
}

/**
 * Avatar initials snap to the ladder by the avatar's own size instead of
 * scaling continuously with it.
 */
export function avatarInitialsType(sizePxValue: number): EliteaTypographyVariant {
  if (sizePxValue <= 24) return 'labelSmall';
  if (sizePxValue <= 32) return 'labelMedium';
  if (sizePxValue <= 48) return 'headingMedium';
  return 'headingLarge';
}

/**
 * The one place the token layer names a CSS variable by hand. A typography
 * variant is a plain style object — MUI offers it no `theme.vars` — so the
 * variable is spelled out, and it MUST carry the prefix of the theme the
 * variant is built into. A theme built under another scope (the Branding
 * page's preview, `--elp-*`) that named `--el-*` here would paint its
 * headings with the OUTER app theme's `text.secondary`: white, from the
 * console's dark scheme, on the preview's light surface.
 */
const paletteVar = (cssVarPrefix: string, path: string): string =>
  `var(--${cssVarPrefix}-palette-${path})`;

/**
 * @param cssVarPrefix the `cssVariables.cssVarPrefix` of the theme this
 *   typography is built into; the app theme's by default.
 */
export function toTypography(
  typography: BrandPack['typography'],
  cssVarPrefix: string = CSS_VAR_PREFIX,
): TypographyVariantsOptions {
  const { fontFamily, fontFamilyMono, baseSize, scale } = typography;
  const ladder = ladderPx(baseSize, scale);
  const variants = {} as Record<EliteaTypographyVariant, CSSProperties>;

  for (const [name, spec] of Object.entries(VARIANTS) as [EliteaTypographyVariant, VariantSpec][]) {
    variants[name] = {
      fontStyle: 'normal',
      fontWeight: spec.fontWeight,
      fontSize: rem(ladder[spec.step]),
      lineHeight: spec.lineHeight,
      letterSpacing: spec.letterSpacing ?? 'normal',
      ...(spec.textTransform === undefined ? {} : { textTransform: spec.textTransform }),
      ...(spec.secondaryText === undefined
        ? {}
        : { color: paletteVar(cssVarPrefix, 'text-secondary') }),
    };
  }

  const aliases: Record<string, TypeScale> = {};
  for (const [stock, target] of Object.entries(STOCK_ALIASES)) {
    aliases[stock] = typeScale(variants[target]);
  }

  return {
    fontFamily,
    fontFamilyMono,
    fontSize: baseSize,
    // MainTheme.js:114 verbatim — disables the ligature substitutions that
    // made identifiers in code-adjacent UI unreadable.
    fontFeatureSettings: '"clig" 0, "liga" 0',
    ...aliases,
    ...variants,
  } as TypographyVariantsOptions;
}
