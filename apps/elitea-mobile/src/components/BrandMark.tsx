import { StyleSheet, View, type ViewProps } from 'react-native';
import { SvgXml } from 'react-native-svg';

import { identity } from '@/config/identity';
import { useDesign } from '@/design/DesignProvider';

/**
 * The brand's logo mark, the app's own fixed identity (docs/design-system.md §5): the
 * `assets/logo/logo-mark.svg` of the brand the app was built for (ADR-0052;
 * `identity.markSvg`). For ELITEA it is the LE-03 mark.
 *
 *  - `mark`:   the mark alone; empty states, the welcome hero, About, the lock and privacy covers.
 *  - `avatar`: the mark in a circle on the theme surface; the fallback avatar of the
 *              conversation's primary agent (24 pt minimum).
 *
 * The mark is vector art, so it is sharp at every size. ELITEA's is a copy of the canonical SVG
 * of `@elitea/client-core/brand` (scripts/brand/elitea-sync.test.mjs keeps them equal). It is never
 * recoloured or tinted with the workspace brand, and it never overlaps or replaces the workspace's
 * own logo (the runtime brand pack, src/theme/BrandLogo.tsx), nor appears on approval, security
 * or destructive surfaces.
 *
 * Accessibility: an image labelled with the brand's short name by default. Pass `label` (e.g. the agent's name when
 * it stands in for an agent avatar), or `decorative` when a heading next to it carries the meaning.
 */

export type BrandMarkVariant = 'mark' | 'avatar';

/** The default accessibility label: the brand's short name. */
export const BRAND_MARK_NAME: string = identity.shortName;

/** The mark's markup. */
export const BRAND_MARK_SVG: string = identity.markSvg;

/** Width over height, from the mark's viewBox (1 when it cannot be read). */
export const BRAND_MARK_ASPECT: number = aspectOf(BRAND_MARK_SVG);

function aspectOf(svg: string): number {
  const box = /\bviewBox="\s*[-\d.]+[\s,]+[-\d.]+[\s,]+([\d.]+)[\s,]+([\d.]+)\s*"/.exec(svg);
  const w = box ? Number(box[1]) : NaN;
  const h = box ? Number(box[2]) : NaN;
  return w > 0 && h > 0 ? w / h : 1;
}

/** Minimum rendered size (pt). */
export const BRAND_MARK_MIN_SIZE = 24;

/** The mark's share of the avatar circle's diameter. */
const AVATAR_MARK_SHARE = 0.62;

export function BrandMark({
  variant = 'mark',
  size = 96,
  label = BRAND_MARK_NAME,
  decorative = false,
  style,
  testID,
}: {
  variant?: BrandMarkVariant;
  /** Rendered width in pt (for `avatar`, the circle's diameter). Clamped to the 24 pt minimum. */
  size?: number;
  label?: string;
  decorative?: boolean;
  /** `View`'s own style type: the web-widened `ViewStyle` (expo/types) does not fit it (#30). */
  style?: ViewProps['style'];
  testID?: string;
}) {
  const { colors } = useDesign();
  const width = Math.max(BRAND_MARK_MIN_SIZE, size);
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
        <SvgXml xml={BRAND_MARK_SVG} width={inner} height={Math.round(inner / BRAND_MARK_ASPECT)} />
      </View>
    );
  }

  const height = Math.round(width / BRAND_MARK_ASPECT);
  return (
    <View testID={testID} {...a11y} style={[{ width, height }, style]}>
      <SvgXml xml={BRAND_MARK_SVG} width={width} height={height} />
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
