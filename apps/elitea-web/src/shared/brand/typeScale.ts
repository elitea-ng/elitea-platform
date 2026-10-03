import type { CSSProperties } from 'react';


/**
 * The read side of the one type scale (`typography.ts` builds it): the helpers
 * component overrides and call sites use to COPY a variant.
 *
 * A module of its own, apart from `typography.ts`, for the bundle: the
 * overrides in the initial chunk and dozens of lazy route chunks import these
 * helpers, and a module shared that widely is split into its own chunk. Kept
 * here, that chunk is a few hundred bytes; inside `typography.ts` it dragged
 * the whole ladder builder out of the theme chunk and into the app's initial
 * set as a separate download (measured: +0.8 KiB gzip over the 300 KiB
 * budget).
 */

/**
 * The eight variants of the one type scale. Declared here, in the leaf
 * module, so `typography.ts` can import it without `typeScale.ts` importing
 * back (dependency-cruiser's no-circular counts type-only edges too);
 * `typography.ts` checks its VARIANTS table against this union.
 *
 * @public Wave-1 surface: unit S1 types `<Typography variant>` call sites with it.
 */
export type EliteaTypographyVariant =
  | 'headingLarge'
  | 'headingMedium'
  | 'headingSmall'
  | 'labelMedium'
  | 'bodyMedium'
  | 'labelSmall'
  | 'bodySmall'
  | 'subtitle';

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
  return Number((small / medium).toFixed(4));
}

/**
 * The pack's monospace family, as an `sx` value callback:
 * `sx={{ fontFamily: monoFontFamily }}`. Code, IDs and secret hints use it
 * instead of the bare generic `'monospace'`, which ignores the brand pack's
 * `fontFamilyMono` (typography spec §2, the code row).
 */
export function monoFontFamily(theme: { typography: { fontFamilyMono: string } }): string {
  return theme.typography.fontFamilyMono;
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
