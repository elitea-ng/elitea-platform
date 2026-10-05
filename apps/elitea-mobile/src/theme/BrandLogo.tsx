import { useEffect, useState } from 'react';
import { Image, StyleSheet, View } from 'react-native';
import { SvgXml } from 'react-native-svg';

import type { BrandImage } from '@/theme/brandPack';
import { fetchBrandRaster } from '@/theme/brandAssets';
import { fetchBrandSvg } from '@/theme/brandSvg';

/**
 * Renders a brand image that `acceptImage` already vetted (workspace origin or
 * inline raster). Renders nothing for null or on a load error; the logo is
 * decoration and never blocks a screen.
 *
 * An SVG is fetched through the origin guard and rendered from its checked
 * markup (src/theme/brandSvg.ts), never with `SvgUri`, which would follow a
 * redirect anywhere and load whatever the document references. A raster URL
 * is likewise downloaded through the guard with redirects refused and shown
 * from its bytes (`data:` URI), never handed to `<Image>` as a URL (SEC-3).
 */
export function BrandLogo({
  image,
  size = 56,
  label,
  fetchSvg = fetchBrandSvg,
  fetchRaster = fetchBrandRaster,
}: {
  image: BrandImage | null;
  size?: number;
  label?: string;
  /** Injectable for tests. */
  fetchSvg?: (uri: string) => Promise<string | null>;
  /** Injectable for tests. */
  fetchRaster?: (uri: string) => Promise<string | null>;
}) {
  // Keyed by URI, so a new image starts clean without a reset inside the effect.
  const [failedUri, setFailedUri] = useState<string | null>(null);
  const [svg, setSvg] = useState<{ uri: string; xml: string | null } | null>(null);
  const [raster, setRaster] = useState<{ uri: string; data: string | null } | null>(null);
  const svgUri = image?.kind === 'svg' ? image.uri : null;
  // An inline data: raster from the pack is shown as is; a URL is downloaded.
  const rasterUri = image?.kind === 'raster' && !image.uri.startsWith('data:') ? image.uri : null;

  useEffect(() => {
    if (rasterUri === null) return;
    let cancelled = false;
    void fetchRaster(rasterUri).then((data) => {
      if (!cancelled) setRaster({ uri: rasterUri, data });
    });
    return () => {
      cancelled = true;
    };
  }, [rasterUri, fetchRaster]);

  useEffect(() => {
    if (svgUri === null) return;
    let cancelled = false;
    void fetchSvg(svgUri).then((xml) => {
      if (!cancelled) setSvg({ uri: svgUri, xml });
    });
    return () => {
      cancelled = true;
    };
  }, [svgUri, fetchSvg]);

  if (image === null || failedUri === image.uri) return null;
  const xml = image.kind === 'svg' && svg?.uri === image.uri ? svg.xml : null;
  if (image.kind === 'svg' && xml === null) return null;
  const rasterSource =
    image.kind !== 'raster' ? null : image.uri.startsWith('data:') ? image.uri : raster?.uri === image.uri ? raster.data : null;
  if (image.kind === 'raster' && rasterSource === null) return null;
  const setFailed = () => setFailedUri(image.uri);
  return (
    <View
      accessible={label !== undefined}
      accessibilityRole="image"
      accessibilityLabel={label}
      style={[styles.box, { width: size, height: size }]}>
      {image.kind === 'svg' ? (
        <SvgXml xml={xml} width={size} height={size} onError={setFailed} />
      ) : (
        <Image
          testID="brand-raster"
          source={{ uri: rasterSource as string }}
          style={{ width: size, height: size }}
          resizeMode="contain"
          onError={setFailed}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  box: { alignItems: 'center', justifyContent: 'center' },
});
