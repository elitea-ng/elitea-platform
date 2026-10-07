import { useMemo } from 'react';
import { StyleSheet, View } from 'react-native';

import { resolveBrand, type BrandInput } from '@/design/brand';
import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { BrandLogo } from '@/theme/BrandLogo';
import type { BrandImage } from '@/theme/brandPack';
import { monogramStyle, Text } from '@/ui';

/** The monogram tile's colours: the workspace's own corrected accent family, else neutral. */
export function markTint(brand: BrandInput | null | undefined, scheme: 'light' | 'dark'): { fill: string; text: string } | null {
  const overrides = resolveBrand(brand ?? null)[scheme];
  if (!overrides.accentSoft || !overrides.accentSoftOn) return null;
  return { fill: overrides.accentSoft, text: overrides.accentSoftOn };
}

/**
 * A workspace's brand mark, or (no mark on the device) the first letter of its host on a tile in
 * THAT workspace's accent (corrected like the app's own accent, so the letter keeps 4.5:1), or a
 * neutral tile when the workspace has no brand. Decoration only: the host is always written
 * next to it, so two workspaces with the same brand can never be mixed up.
 */
export function WorkspaceMark({
  origin,
  image,
  brand,
  size = 32,
}: {
  origin: string;
  image: BrandImage | null;
  /** The workspace's brand accent inputs (`WorkspaceSummary.brand`, or the active theme's). */
  brand?: BrandInput | null;
  size?: number;
}) {
  const { colors, scheme } = useDesign();
  const tint = useMemo(() => markTint(brand, scheme), [brand, scheme]);
  if (image !== null) {
    return (
      <View style={[styles.center, { width: size, height: size }]}>
        <BrandLogo image={image} size={size} />
      </View>
    );
  }
  const letter = displayHost(origin).charAt(0).toUpperCase() || '?';
  return (
    <View
      accessible={false}
      importantForAccessibility="no-hide-descendants"
      style={[
        styles.tile,
        styles.center,
        { width: size, height: size, borderRadius: size / 4, backgroundColor: tint?.fill ?? colors.surfaceSunken },
      ]}>
      <Text maxFontSizeMultiplier={1} color={tint?.text ?? colors.textSecondary} style={[monogramStyle(size, 0.5), styles.letter]}>
        {letter}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  center: { alignItems: 'center', justifyContent: 'center' },
  tile: { overflow: 'hidden', borderCurve: 'continuous' },
  letter: { fontWeight: '700' },
});
