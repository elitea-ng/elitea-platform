import { DEFAULT_PRODUCT, ELITEA_MARK_ASPECT, ELITEA_MARK_SVG } from '@elitea/client-core/brand';
import { StyleSheet, View, type ViewProps } from 'react-native';
import { SvgXml } from 'react-native-svg';

import { useDesign } from '@/design/DesignProvider';

/**
 * The Elitea logo mark, the product's own fixed identity (docs/design-system.md §5).
 *
 *  - `mark`:   the mark alone; empty states, the welcome hero, About, the lock and privacy covers.
 *  - `avatar`: the mark in a circle on the theme surface; the fallback avatar of the
 *              conversation's primary agent (24 pt minimum).
 *
 * The mark is vector art, the canonical SVG of `@elitea/client-core/brand` (libs/ts/client-core,
 * the same source as the web app's copies), so it is sharp at every size. It is never
 * recoloured or tinted with the workspace brand, and it never overlaps or replaces the workspace's
 * own logo (the runtime brand pack, src/theme/BrandLogo.tsx), nor appears on approval, security
 * or destructive surfaces.
 *
 * Accessibility: an image labelled "Elitea" by default. Pass `label` (e.g. the agent's name when
 * it stands in for an agent avatar), or `decorative` when a heading next to it carries the meaning.
 */

export type EliteaMarkVariant = 'mark' | 'avatar';

export const ELITEA_MARK_NAME: string = DEFAULT_PRODUCT.shortName;

/** Minimum rendered size (pt). */
export const ELITEA_MARK_MIN_SIZE = 24;

/** The mark's share of the avatar circle's diameter. */
const AVATAR_MARK_SHARE = 0.62;

export function EliteaMark({
  variant = 'mark',
  size = 96,
  label = ELITEA_MARK_NAME,
  decorative = false,
  style,
  testID,
}: {
  variant?: EliteaMarkVariant;
  /** Rendered width in pt (for `avatar`, the circle's diameter). Clamped to the 24 pt minimum. */
  size?: number;
  label?: string;
  decorative?: boolean;
  /** `View`'s own style type: the web-widened `ViewStyle` (expo/types) does not fit it (#30). */
  style?: ViewProps['style'];
  testID?: string;
}) {
  const { colors } = useDesign();
  const width = Math.max(ELITEA_MARK_MIN_SIZE, size);
  const a11y = decorative
    ? ({ accessible: false, importantForAccessibility: 'no-hide-descendants', accessibilityElementsHidden: true } as const)
    : ({ accessible: true, accessibilityRole: 'image', accessibilityLabel: label } as const);

  if (variant === 'avatar') {
    const inner = Math.round(width * AVATAR_MARK_SHARE);
    return (
      <View
        testID={testID}
        {...a11y}
        style={[
          styles.avatar,
          { width, height: width, borderRadius: width / 2, backgroundColor: colors.surface, borderColor: colors.separator },
          style,
        ]}>
        <SvgXml xml={ELITEA_MARK_SVG} width={inner} height={Math.round(inner / ELITEA_MARK_ASPECT)} />
      </View>
    );
  }

  const height = Math.round(width / ELITEA_MARK_ASPECT);
  return (
    <View testID={testID} {...a11y} style={[{ width, height }, style]}>
      <SvgXml xml={ELITEA_MARK_SVG} width={width} height={height} />
    </View>
  );
}

const styles = StyleSheet.create({
  avatar: {
    overflow: 'hidden',
    alignItems: 'center',
    justifyContent: 'center',
    borderWidth: StyleSheet.hairlineWidth,
    borderCurve: 'continuous',
  },
});
