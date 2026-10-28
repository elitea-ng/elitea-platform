import { type ReactNode, useMemo } from 'react';

import type { EmotionCache } from '@emotion/cache';
import { CacheProvider } from '@emotion/react';
import CssBaseline from '@mui/material/CssBaseline';
import GlobalStyles from '@mui/material/GlobalStyles';
import { ThemeProvider } from '@mui/material/styles';

import {
  type BrandPack,
  DEFAULT_BRAND_PACK,
  DEFAULT_COLOR_SCHEME,
  INIT_COLOR_SCHEME_PROPS,
  buildEliteaTheme,
} from '@/shared/brand';

import { BrandDocumentHead } from './BrandDocumentHead';

export interface BrandThemeProviderProps {
  children: ReactNode;
  /**
   * The resolved brand pack. Channel C (`GET /api/v2/branding/bootstrap.js`,
   * unit W3) is now wired: `AppProviders` passes
   * `shared/brand/channelC.ts`'s `resolveBrandPack()`, which validates
   * `window.elitea_brand` and falls back to the compiled-in
   * `DEFAULT_BRAND_PACK` (channel A) when no valid pack was served. Until
   * that wiring landed this prop had no caller at all and the default below
   * always won, so nothing about the running app was pack-driven (issue #136
   * C). The default is retained for tests and stories that render the
   * provider standalone.
   */
  pack?: BrandPack;
}

/**
 * Tier 2 theme wiring (spec §4.2; §9.3 unit R2 task 2).
 *
 * `modeStorageKey` is passed to `ThemeProvider` explicitly and set to the
 * SAME `'el-mode'` key the static first-paint script reads (`index.html`
 * inline; `entries/admin/public/assets/scheme-init.js` for the admin entry).
 * `ThemeProvider`'s own default (`mui-mode`) does not match that script, and
 * if the two disagreed the runtime provider would read/write a different
 * localStorage key than the one the anti-flash script consulted before it
 * mounted — silently reintroducing the flash on every subsequent visit.
 *
 * MUI's `<InitColorSchemeScript>` used to be rendered here as well. It was
 * VERIFIED — by rendering it under `@testing-library/react` and inspecting
 * the DOM — to emit NO `<script>` element at all for this app: the component
 * renders its script only while `useSyncExternalStore` reports "server
 * render or matching hydration render", which is never true under
 * `createRoot` (spec N6 bans SSR/hydration for this app). It has been removed
 * (ADR-0024 WP3) in favour of the static script, which is the one real
 * anti-flash mechanism this bootstrap can have. `BrandThemeProvider.test.tsx`
 * pins that no runtime script carrying `el-mode` is emitted, so the static
 * one stays the single source.
 *
 * `BrandDocumentHead` carries the `<head>` half of the pack: `@font-face`
 * rules from `typography.fontFaces` and the custom favicon.
 */
/**
 * Right-to-left languages: the desktop entry applies the language (which sets
 * `dir="rtl"` on the document) and installs an emotion cache whose RTL plugin
 * mirrors physical properties (`paddingLeft`, `left`, `textAlign: 'left'`, …)
 * app-wide (`entries/desktop/rtlStyleCache.ts`), both before the first render.
 * The plugin is loaded only then, so the web app's bundle never carries it.
 * Left-to-right keeps emotion's default cache, so its styles are untouched.
 */
let rtlCache: EmotionCache | undefined;

/**
 * Icons whose meaning is a direction (back/forward, a disclosure, the next
 * page) point the other way in RTL. MUI names each icon in `data-testid`.
 * Brands, clocks, media controls and text are never mirrored.
 */
const MIRRORED_ICONS = ['ChevronRight', 'ChevronLeft', 'ArrowBack', 'ArrowForward', 'ArrowBackIos', 'ArrowForwardIos', 'KeyboardArrowRight', 'KeyboardArrowLeft', 'NavigateNext', 'NavigateBefore', 'FirstPage', 'LastPage', 'Undo', 'Redo', 'Logout', 'Login', 'OpenInNew']
  .map((name) => `[dir='rtl'] [data-testid='${name}Icon'], [dir='rtl'] [data-testid='${name}OutlinedIcon']`)
  .join(', ');

export function installRtlStyleCache(cache: EmotionCache): void {
  rtlCache = cache;
}

export function BrandThemeProvider({ children, pack = DEFAULT_BRAND_PACK }: BrandThemeProviderProps) {
  const rtl = rtlCache !== undefined && document.documentElement.dir === 'rtl';
  const theme = useMemo(() => buildEliteaTheme(pack, undefined, rtl ? 'rtl' : 'ltr'), [pack, rtl]);

  const themed = (
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
      modeStorageKey={INIT_COLOR_SCHEME_PROPS.modeStorageKey}
    >
      <CssBaseline enableColorScheme />
      <BrandDocumentHead pack={pack} />
      {children}
    </ThemeProvider>
  );
  return rtl && rtlCache !== undefined ? (
    <CacheProvider value={rtlCache}>
      {/* `scale`, not `transform`: it composes with an icon's own rotation (an expanded disclosure). */}
      <GlobalStyles styles={{ [MIRRORED_ICONS]: { scale: '-1 1' } }} />
      {themed}
    </CacheProvider>
  ) : (
    themed
  );
}
