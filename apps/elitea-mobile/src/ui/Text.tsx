import { Text as RNText, type TextProps as RNTextProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import type { SemanticColors } from '@/design/tokens';

import type { TypeRole } from './type';

/** Text colours a label may take; each is proven ≥ 4.5:1 on the surfaces it is used on. */
export type TextTone =
  | 'primary'
  | 'secondary'
  | 'tertiary'
  | 'accent'
  | 'onAccent'
  | 'accentSoftOn'
  | 'danger'
  | 'dangerOn'
  | 'success'
  | 'warning'
  | 'secondaryBrand';

const TONE: Record<TextTone, keyof SemanticColors> = {
  primary: 'textPrimary',
  secondary: 'textSecondary',
  tertiary: 'textTertiary',
  accent: 'accent',
  onAccent: 'accentOn',
  accentSoftOn: 'accentSoftOn',
  danger: 'danger',
  dangerOn: 'dangerOn',
  success: 'success',
  warning: 'warning',
  secondaryBrand: 'secondary',
};

export function toneColor(colors: SemanticColors, tone: TextTone): string {
  return colors[TONE[tone]] as string;
}

export interface TextProps extends RNTextProps {
  /** The type role (docs/design-system.md §1.7). Default `body`. */
  variant?: TypeRole;
  /** Semantic colour. Default `primary`. Ignored when `color` is set. */
  tone?: TextTone;
  /** An explicit colour (a token read from `useDesign()`), for the rare non-tone case. */
  color?: string;
  /** Use the role's weight +1 step (e.g. an unread title). */
  emphasized?: boolean;
  /** Role a heading carries for screen readers (`accessibilityRole="header"`). */
  heading?: boolean;
}

/**
 * Text set in a role of the type ramp: the platform size and line height, the role's scale
 * cap (content scales with Dynamic Type / font scale; chrome is capped), the workspace brand
 * face on display/title/headline only, and a semantic colour. Content is never kept from
 * scaling (`allowFontScaling` stays on).
 */
export function Text({ variant = 'body', tone = 'primary', color, emphasized, heading, style, maxFontSizeMultiplier, ...rest }: TextProps) {
  const { typography, colors } = useDesign();
  const { maxFontSizeMultiplier: cap, ...role } = typography[variant];
  return (
    <RNText
      accessibilityRole={heading ? 'header' : rest.accessibilityRole}
      maxFontSizeMultiplier={maxFontSizeMultiplier ?? cap}
      {...rest}
      style={[role, emphasized && { fontWeight: role.fontWeight === '400' ? '600' : '700' }, { color: color ?? toneColor(colors, tone) }, style]}
    />
  );
}
