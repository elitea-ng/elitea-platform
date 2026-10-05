import * as Font from 'expo-font';
import { useEffect, useState } from 'react';

import { log } from '@/lib/log';
import { downloadBrandFont } from '@/theme/brandAssets';
import { useBrandTheme } from '@/theme/brandStore';
import type { BrandFontFace } from '@/theme/brandPack';

/**
 * Loads the brand's same-origin font face (already vetted by
 * `mapBrandPack`: same origin, .ttf/.otf, one face) and returns the family to
 * render headings with, or undefined for the system font. A face that fails to
 * load is simply not used. The face is downloaded through the origin guard
 * with redirects refused (src/theme/brandAssets.ts) and expo-font loads the
 * local file, never the URL (it would follow a redirect to any host, SEC-3).
 */
const loaded = new Set<string>();

export async function loadBrandFaces(
  faces: readonly BrandFontFace[],
  loader: (map: Record<string, { uri: string }>) => Promise<void> = (map) => Font.loadAsync(map),
  download: (face: BrandFontFace) => Promise<string | null> = (face) => downloadBrandFont(face),
): Promise<string[]> {
  const ok: string[] = [];
  for (const face of faces) {
    if (loaded.has(face.alias)) {
      ok.push(face.alias);
      continue;
    }
    try {
      const local = await download(face);
      if (local === null) continue;
      await loader({ [face.alias]: { uri: local } });
      loaded.add(face.alias);
      ok.push(face.alias);
    } catch (error) {
      log.info('brand font not loaded', { error });
    }
  }
  return ok;
}

export function useBrandFontFamily(): string | undefined {
  const theme = useBrandTheme();
  const family = theme?.fontFamily ?? null;
  const [ready, setReady] = useState<string | null>(family !== null && loaded.has(family) ? family : null);
  useEffect(() => {
    if (theme === null || family === null) return;
    let cancelled = false;
    void loadBrandFaces(theme.fontFaces).then((ok) => {
      if (!cancelled && ok.includes(family)) setReady(family);
    });
    return () => {
      cancelled = true;
    };
  }, [theme, family]);
  return family !== null && ready === family ? family : undefined;
}

/** Test helper. */
export function __resetBrandFontsForTests(): void {
  loaded.clear();
}
