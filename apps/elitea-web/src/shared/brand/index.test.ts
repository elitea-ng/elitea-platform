import { describe, expect, it } from 'vitest';

import * as brand from './index';

/**
 * The barrel is the theming layer's contract with the rest of the app: every
 * Wave-1 unit (R2's providers, S1's stories, the admin brand editor) imports
 * from here and nowhere else. Pinning the surface makes a rename or an
 * accidental removal a failing test rather than a downstream mystery, and it
 * keeps the module in the dependency graph knip walks.
 */
const PUBLIC_SURFACE = [
  'BRAND_ANCHOR_TOKEN',
  'BREAKPOINT_VALUES',
  'BRAND_PACK_GLOBAL',
  'BrandPack',
  'COLOR_SCHEME_ATTRIBUTE',
  'COLOR_SCHEME_SELECTOR',
  'CSS_VAR_PREFIX',
  'DEFAULT_BRAND_PACK',
  'DEFAULT_COLOR_SCHEME',
  'FONT_FACE_STYLE_ATTRIBUTE',
  'INIT_COLOR_SCHEME_PROPS',
  // The admin Branding page's live-preview path (ADR-0024 WP4).
  'PREVIEW_ROOT_CLASS',
  'PREVIEW_SCHEME_ATTRIBUTE',
  'PREVIEW_THEME_SCOPE',
  'WCAG_AA_NORMAL_TEXT',
  'buildEliteaTheme',
  'catalogueFor',
  'contrastRatio',
  'docsBaseUrl',
  'docsLink',
  'fontFaceRule',
  'fontFaceStylesheet',
  'hasServedBrandPack',
  'hueDeltaFor',
  'isSameOriginAssetPath',
  'parseBrandPack',
  'primaryContrastOf',
  'productFullName',
  'productName',
  'resolveBrandAsset',
  'resolveBrandPack',
  'resolveScheme',
  'supportEmail',
  'supportUrl',
  'swatchesOf',
  'toMuiPalette',
] as const;

describe('shared/brand public surface', () => {
  it('exports exactly the documented set', () => {
    expect(Object.keys(brand).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });

  it('builds a usable theme straight from the barrel', () => {
    const theme = brand.buildEliteaTheme(brand.DEFAULT_BRAND_PACK);
    expect(theme.cssVarPrefix).toBe(brand.CSS_VAR_PREFIX);
    expect(theme.colorSchemeSelector).toBe(brand.COLOR_SCHEME_SELECTOR);
  });

  it('keeps the anti-flash script props in step with the theme', () => {
    // The MUI 9.2 caveat this guards: InitColorSchemeScript's `attribute` and
    // `defaultMode` must match `colorSchemeSelector` and `defaultColorScheme`,
    // or the script writes an attribute the stylesheet never matches.
    expect(brand.INIT_COLOR_SCHEME_PROPS.attribute).toBe(brand.COLOR_SCHEME_SELECTOR);
    expect(brand.INIT_COLOR_SCHEME_PROPS.defaultMode).toBe(brand.DEFAULT_COLOR_SCHEME);
    expect(brand.COLOR_SCHEME_SELECTOR).toContain(brand.COLOR_SCHEME_ATTRIBUTE);
  });
});
