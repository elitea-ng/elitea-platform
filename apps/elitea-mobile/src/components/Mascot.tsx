import { Image, StyleSheet, View, type ImageSourcePropType, type ViewProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { colors as zefirColors } from '@/design/tokens';

/**
 * Agent Zefirca, the Agent Zefir mascot (docs/design-system.md §5).
 *
 *  - `avatar`: the head in a circle on a theme surface; agent-avatar fallback (24 pt minimum).
 *  - `bust`:   head, hologram hand and coat to the belt; headers, voice, lock cover.
 *  - `full`:   full figure without the pedestal; empty states, About.
 *  - `hero`:   full figure on the mint pedestal; the welcome / connect hero only.
 *
 * The artwork is never recoloured, tinted with the workspace brand or inverted:
 * dark mode only adds a soft rim halo. It never overlaps the workspace logo
 * and is never shown on approval, security or destructive surfaces.
 *
 * Accessibility: an image labelled "Agent Zefirca" by default. Pass `label`
 * (e.g. the agent's name when it stands in for an agent avatar), or
 * `decorative` when a heading next to it already carries the meaning.
 */

export type MascotVariant = 'avatar' | 'bust' | 'full' | 'hero';

export const MASCOT_NAME = 'Agent Zefirca';

/** Minimum rendered size (pt) and the clear space around the figure, as a share of its width. */
export const MASCOT_MIN_SIZE = 24;
export const MASCOT_CLEAR_SPACE = 0.15;

const ASSETS: Record<MascotVariant, { source: ImageSourcePropType; aspect: number }> = {
  avatar: { source: require('../../assets/images/mascot/avatar-head.png'), aspect: 1 },
  bust: { source: require('../../assets/images/mascot/mascot-bust.png'), aspect: 1080 / 884 },
  full: { source: require('../../assets/images/mascot/mascot-full.png'), aspect: 574 / 676 },
  hero: { source: require('../../assets/images/mascot/mascot-hero.png'), aspect: 655 / 770 },
};

/**
 * Largest rendered width (pt) that stays sharp on a @3x screen: the widest file of the
 * variant divided by 3. `full` and `hero` ship @1x/@2x/@3x; `bust` (1080 px) and `avatar`
 * (384 px) ship one file each, so keep them at or below this size (the documented uses in
 * docs/design-system.md §5 are 28-96 pt). Above it the image is upscaled and goes soft.
 */
export const MASCOT_MAX_SHARP_SIZE: Record<MascotVariant, number> = { avatar: 128, bust: 360, full: 574, hero: 655 };

/** Width/height of a variant's artwork. */
export function mascotAspect(variant: MascotVariant): number {
  return ASSETS[variant].aspect;
}

const DARK_HALO = 'rgba(255, 255, 255, 0.10)';

export function Mascot({
  variant = 'bust',
  size = 96,
  label = MASCOT_NAME,
  decorative = false,
  style,
  testID,
}: {
  variant?: MascotVariant;
  /**
   * Rendered width in pt (for `avatar`, the circle's diameter). Clamped to the 24 pt minimum;
   * sharp up to `MASCOT_MAX_SHARP_SIZE[variant]`.
   */
  size?: number;
  label?: string;
  decorative?: boolean;
  /** `View`'s own style type: the web-widened `ViewStyle` (expo/types) does not fit it (#30). */
  style?: ViewProps['style'];
  testID?: string;
}) {
  const { colors, scheme } = useDesign();
  // The avatar disc is Zefir's own soft rose, never the workspace accent: the mascot is not brandable.
  const disc = zefirColors[scheme].accentSoft;
  const width = Math.max(MASCOT_MIN_SIZE, size);
  const { source, aspect } = ASSETS[variant];
  const height = Math.round(width / aspect);
  const a11y = decorative
    ? ({ accessible: false, importantForAccessibility: 'no-hide-descendants', accessibilityElementsHidden: true } as const)
    : ({ accessible: true, accessibilityRole: 'image', accessibilityLabel: label } as const);

  if (variant === 'avatar') {
    return (
      <View
        testID={testID}
        {...a11y}
        style={[
          styles.avatar,
          {
            width,
            height: width,
            borderRadius: width / 2,
            backgroundColor: disc,
            borderColor: scheme === 'dark' ? DARK_HALO : colors.separator,
          },
          style,
        ]}>
        <Image source={source} style={{ width, height: width }} resizeMode="cover" />
      </View>
    );
  }

  return (
    <View
      testID={testID}
      {...a11y}
      style={[{ width, height }, scheme === 'dark' && styles.halo, style]}>
      <Image source={source} style={{ width, height }} resizeMode="contain" />
    </View>
  );
}

const styles = StyleSheet.create({
  avatar: {
    overflow: 'hidden',
    borderWidth: StyleSheet.hairlineWidth,
    borderCurve: 'continuous',
  },
  halo: {
    // iOS draws the halo as a soft white glow around the figure's alpha; Android has no coloured
    // shadow for images and shows the artwork as is.
    shadowColor: '#FFFFFF',
    shadowOpacity: 0.1,
    shadowRadius: 12,
    shadowOffset: { width: 0, height: 0 },
  },
});
