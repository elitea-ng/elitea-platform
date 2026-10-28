/**
 * The emotion cache for a right-to-left language: its RTL plugin mirrors
 * every physical style (`paddingLeft` → `paddingRight`, …), and the prefixer
 * keeps the vendor prefixes emotion's default cache adds. `./main.tsx` loads
 * this module only for an RTL language (`app/providers/BrandThemeProvider.tsx`).
 */
import createCache, { type EmotionCache } from '@emotion/cache';
import rtlPlugin from '@mui/stylis-plugin-rtl';
import { prefixer } from 'stylis';

export function createRtlStyleCache(): EmotionCache {
  return createCache({ key: 'muirtl', stylisPlugins: [prefixer, rtlPlugin] });
}
