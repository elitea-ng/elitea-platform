import { SymbolView, type SymbolWeight } from 'expo-symbols';
import { useWindowDimensions, type ViewProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { ICONS, filledSymbol, type IconName } from './icons';
import { MATERIAL_WEIGHT } from './materialWeight';
import { toneColor, type TextTone } from './Text';

export interface IconProps {
  name: IconName;
  /** Visual size in pt/dp: 20 inline, 22 in icon buttons, 24 in the tab bar (§1.8). */
  size?: number;
  tone?: TextTone;
  /** An explicit colour; wins over `tone`. */
  color?: string;
  /** The filled SF Symbol (selected states only). */
  selected?: boolean;
  /** SF Symbol weight; match the adjacent text (regular next to body, semibold next to headline). */
  weight?: SymbolWeight;
  /**
   * Speaks the icon. Without it the icon is decorative (hidden from VoiceOver/TalkBack), which
   * is right whenever a text label or the control's own label carries the meaning.
   */
  label?: string;
  /**
   * Grow with the text size (Dynamic Type / font scale), as SF Symbols next to text do, up to
   * `ICON_MAX_SCALE`. Default true; pass false inside a fixed frame that cannot grow.
   */
  scales?: boolean;
  style?: ViewProps['style'];
  testID?: string;
}

/** Icons grow with the text size up to this factor, so they stay balanced with their labels. */
export const ICON_MAX_SCALE = 1.5;

export function scaledIconSize(size: number, fontScale: number): number {
  return Math.round(size * Math.min(Math.max(fontScale || 1, 1), ICON_MAX_SCALE));
}

/**
 * A platform icon by semantic name: an SF Symbol on iOS (expo-symbols `SymbolView`), a Material
 * Symbol (Rounded) on Android. Tinted with a token; never an emoji or a Unicode glyph.
 */
export function Icon({ name, size: base = 20, tone = 'primary', color, selected = false, weight = 'regular', label, scales = true, style, testID }: IconProps) {
  const { colors } = useDesign();
  const { fontScale } = useWindowDimensions();
  const size = scales ? scaledIconSize(base, fontScale) : base;
  const [sf, md] = ICONS[name];
  const a11y = label
    ? ({ accessible: true, accessibilityRole: 'image', accessibilityLabel: label } as const)
    : ({ accessible: false, importantForAccessibility: 'no-hide-descendants', accessibilityElementsHidden: true } as const);
  return (
    <SymbolView
      testID={testID}
      {...a11y}
      name={{ ios: selected ? filledSymbol(name) : sf, android: md, web: md }}
      size={size}
      tintColor={color ?? toneColor(colors, tone)}
      weight={MATERIAL_WEIGHT ? { ios: weight, android: MATERIAL_WEIGHT } : weight}
      scale="medium"
      style={[{ width: size, height: size }, style]}
    />
  );
}
