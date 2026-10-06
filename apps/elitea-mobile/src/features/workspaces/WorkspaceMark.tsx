import { StyleSheet, Text, View } from 'react-native';

import { usePalette } from '@/constants/use-palette';
import { displayHost } from '@/features/connect/connectFlow';
import { BrandLogo } from '@/theme/BrandLogo';
import type { BrandImage } from '@/theme/brandPack';

/**
 * A workspace's brand mark, or (unbranded) the first letter of its host on a
 * neutral tile. Decoration only: the host is always written next to it, so
 * two workspaces with the same brand can never be mixed up.
 */
export function WorkspaceMark({ origin, image, size = 32 }: { origin: string; image: BrandImage | null; size?: number }) {
  if (image !== null) {
    return (
      <View style={[styles.center, { width: size, height: size }]}>
        <BrandLogo image={image} size={size} />
      </View>
    );
  }
  return <Initial origin={origin} size={size} />;
}

function Initial({ origin, size }: { origin: string; size: number }) {
  const palette = usePalette();
  const letter = displayHost(origin).charAt(0).toUpperCase() || '?';
  return (
    <View
      style={[
        styles.tile,
        styles.center,
        { width: size, height: size, borderRadius: size / 4, backgroundColor: palette.backgroundElement },
      ]}>
      <Text style={{ color: palette.textSecondary, fontSize: size * 0.5, fontWeight: '700' }}>{letter}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  center: { alignItems: 'center', justifyContent: 'center' },
  tile: { overflow: 'hidden' },
});
