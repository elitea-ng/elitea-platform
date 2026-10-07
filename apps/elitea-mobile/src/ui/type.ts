import { Platform } from 'react-native';

import { typography, type TypePlatform, type TypeRole, type TypeStyle } from '@/design/typography';

/**
 * A role's text style as a plain object (not RN's `TextStyle`, which Expo's web types widen so
 * that it no longer fits the native `style` props, #30): size, line height and face, but NO
 * weight. A static style that spreads a role sets its own weight when it wants one; otherwise it
 * is regular (F4: Android's subheadline/caption are medium label roles, and spreading their
 * weight turned paragraph text medium). `<Text variant>` still applies the role's weight.
 */
export type RampStyle = Omit<TypeStyle, 'maxFontSizeMultiplier' | 'fontWeight'>;

/**
 * The type ramp as static text styles, for `StyleSheet.create` (docs/design-system.md §1.7).
 *
 * No screen writes a `fontSize` of its own: a style spreads a role (`...ramp.body`) and adds
 * only colour, alignment or a weight. `src/ui/noHardcodedFontSize.test.ts` holds the
 * line. Sizes are the standard-size defaults; the OS text size (Dynamic Type / Android font
 * scale) multiplies them. Text that needs the per-role cap uses `<Text variant>` (src/ui/Text),
 * or passes `maxScale.<role>` as `maxFontSizeMultiplier`.
 *
 * The workspace brand font is not part of these static styles: it is applied by `<Text>` (and
 * `useDesign().typography`) on the display/title/headline roles only.
 */
export type { TypeRole };

const platform: TypePlatform = Platform.OS === 'ios' ? 'ios' : 'android';

/** The ramp's text styles for a platform, without the scale cap (not a style prop) or the weight. */
export function textStyles(on: TypePlatform): Record<TypeRole, RampStyle> {
  const ramp = typography(on);
  const out = {} as Record<TypeRole, RampStyle>;
  for (const role of Object.keys(ramp) as TypeRole[]) {
    const { maxFontSizeMultiplier: _cap, fontWeight: _weight, ...style } = ramp[role];
    out[role] = style;
  }
  return out;
}

/** The per-role `maxFontSizeMultiplier` (chrome is capped, content barely). */
export function maxScales(on: TypePlatform): Record<TypeRole, number> {
  const ramp = typography(on);
  const out = {} as Record<TypeRole, number>;
  for (const role of Object.keys(ramp) as TypeRole[]) out[role] = ramp[role].maxFontSizeMultiplier;
  return out;
}

export const ramp = textStyles(platform);
export const maxScale = maxScales(platform);

/**
 * A monogram letter sized to its tile (workspace mark, agent icon): a glyph, not text of the
 * ramp, so it stays proportional to the fixed tile and does not scale with the text size.
 * Pass `maxFontSizeMultiplier={1}` with it.
 */
export function monogramStyle(tile: number, ratio: number): { fontSize: number; lineHeight: number } {
  return { fontSize: Math.round(tile * ratio), lineHeight: Math.round(tile * ratio * 1.2) };
}
