import type { TextStyle } from 'react-native';

/**
 * Type ramp (docs/design-system.md §1.7): the iOS text styles and the
 * Material 3 type scale, rendered with the system faces (SF Pro / Roboto).
 * Sizes are the defaults at the standard text size; Dynamic Type / font scale
 * multiplies them, capped per role by `maxFontSizeMultiplier`.
 */

export type TypeRole =
  | 'display'
  | 'title1'
  | 'title2'
  | 'title3'
  | 'headline'
  | 'body'
  | 'callout'
  | 'subheadline'
  | 'footnote'
  | 'caption'
  | 'label'
  | 'code';

export type TypePlatform = 'ios' | 'android';

type Weight = NonNullable<TextStyle['fontWeight']>;

interface Spec {
  size: number;
  line: number;
  weight: Weight;
}

const RAMP: Record<TypeRole, { ios: Spec; android: Spec; maxScale: number }> = {
  display: { ios: { size: 34, line: 41, weight: '700' }, android: { size: 36, line: 44, weight: '500' }, maxScale: 1.6 },
  title1: { ios: { size: 28, line: 34, weight: '700' }, android: { size: 28, line: 36, weight: '500' }, maxScale: 1.8 },
  title2: { ios: { size: 22, line: 28, weight: '700' }, android: { size: 22, line: 28, weight: '500' }, maxScale: 2.0 },
  title3: { ios: { size: 20, line: 25, weight: '600' }, android: { size: 18, line: 24, weight: '500' }, maxScale: 2.0 },
  headline: { ios: { size: 17, line: 22, weight: '600' }, android: { size: 16, line: 24, weight: '500' }, maxScale: 2.2 },
  body: { ios: { size: 17, line: 22, weight: '400' }, android: { size: 16, line: 24, weight: '400' }, maxScale: 2.4 },
  callout: { ios: { size: 16, line: 21, weight: '400' }, android: { size: 14, line: 20, weight: '400' }, maxScale: 2.4 },
  subheadline: { ios: { size: 15, line: 20, weight: '400' }, android: { size: 14, line: 20, weight: '500' }, maxScale: 2.2 },
  footnote: { ios: { size: 13, line: 18, weight: '400' }, android: { size: 12, line: 16, weight: '400' }, maxScale: 2.0 },
  caption: { ios: { size: 12, line: 16, weight: '400' }, android: { size: 12, line: 16, weight: '500' }, maxScale: 1.8 },
  label: { ios: { size: 11, line: 13, weight: '600' }, android: { size: 11, line: 16, weight: '500' }, maxScale: 1.5 },
  code: { ios: { size: 15, line: 20, weight: '400' }, android: { size: 14, line: 20, weight: '400' }, maxScale: 2.0 },
};

/** Roles a workspace brand font may take over. Body and metadata always stay system. */
export const BRAND_FONT_ROLES: readonly TypeRole[] = ['display', 'title1', 'title2', 'title3', 'headline'];

/** Above this font scale, horizontal rows stack vertically (§1.7). */
export const ACCESSIBILITY_LAYOUT_SCALE = 1.35;

export interface TypeStyle {
  fontSize: number;
  lineHeight: number;
  fontWeight: Weight;
  fontFamily?: string;
  /** Pass to `<Text maxFontSizeMultiplier>`. Never disable font scaling on content. */
  maxFontSizeMultiplier: number;
}

export type Typography = Record<TypeRole, TypeStyle>;

/**
 * The ramp for `platform`. `brandFont` (an alias registered by the brand
 * font loader) replaces the display and title faces only.
 */
export function typography(platform: TypePlatform, brandFont: string | null = null): Typography {
  const out = {} as Typography;
  for (const role of Object.keys(RAMP) as TypeRole[]) {
    const spec = RAMP[role][platform];
    const style: TypeStyle = {
      fontSize: spec.size,
      lineHeight: spec.line,
      fontWeight: spec.weight,
      maxFontSizeMultiplier: RAMP[role].maxScale,
    };
    if (role === 'code') style.fontFamily = platform === 'ios' ? 'Menlo' : 'monospace';
    else if (brandFont && BRAND_FONT_ROLES.includes(role)) style.fontFamily = brandFont;
    else if (role === 'display' && platform === 'ios') style.fontFamily = 'ui-rounded';
    out[role] = style;
  }
  return out;
}
