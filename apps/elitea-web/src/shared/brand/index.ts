/**
 * Public surface of the brand/token layer (spec §4; §9.3 unit T1).
 *
 * This is the ONLY module the rest of the app imports for theming. Nothing
 * here runs at import time except the default pack's own zod parse, so a
 * broken committed pack fails loudly at module load rather than at first
 * paint.
 *
 * The `@public` annotations mark exports that exist for Wave-1 consumers
 * (R2's provider tree, S1's stories, W3's pack round-trip) and therefore have
 * no in-repo importer yet — the convention unit F4 established for knip.
 */

/** @public Wave-1 surface — R2 builds the app theme from the resolved pack. */
export { buildEliteaTheme, BREAKPOINT_VALUES } from './buildTheme';
export type { EliteaTheme } from './buildTheme';

/** @public Wave-1 surface — R2's InitColorSchemeScript + scheme toggle. */
export {
  COLOR_SCHEME_ATTRIBUTE,
  COLOR_SCHEME_SELECTOR,
  CSS_VAR_PREFIX,
  DEFAULT_COLOR_SCHEME,
  INIT_COLOR_SCHEME_PROPS,
} from './constants';

/** @public Wave-1 surface — channel A floor; also the derivation reference. */
export { DEFAULT_BRAND_PACK, BRAND_ANCHOR_TOKEN } from './tokens';

/** Channel C (spec §4.3) — the served per-deployment pack; see `channelC.ts`. */
export { BRAND_PACK_GLOBAL, parseBrandPack, resolveBrandPack } from './channelC';

/** Brand-derived outbound links (ADR-0024 WP8) — every docs URL and support address comes from here. */
export { docsBaseUrl, docsLink, supportEmail, supportUrl } from './brandLinks';

// ADR-0052 W3.2: the product name every message and label reads.
export { productFullName, productName } from './productName';

/** ADR-0024 WP3 — `pack.assets.*` consumption; see `assets.ts` for the custom-vs-default rule. */
export { hasServedBrandPack, resolveBrandAsset } from './assets';
export type { ResolvedBrandAsset } from './assets';

/** ADR-0024 WP3 — `typography.fontFaces` → `@font-face`; see `fontFaces.ts`. */
export { FONT_FACE_STYLE_ATTRIBUTE, fontFaceRule, fontFaceStylesheet, isSameOriginAssetPath } from './fontFaces';

/** @public Wave-1 surface — W3/channel B+C validate incoming packs with this. */
export { BrandPack } from './schema';
export type { BrandAssetKey, BrandFontFace, BrandInput, SchemeRecord } from './schema';

/** @public Wave-1 surface — the admin brand editor's live-preview path (§4.3 E). */
export { toMuiPalette, resolveScheme, hueDeltaFor, catalogueFor } from './toMuiPalette';
export type { SchemeName } from './toMuiPalette';

/** The admin Branding page's live-preview path (§4.3 E, ADR-0024 WP4). */
export type { ThemeScope } from './buildTheme';
export {
  PREVIEW_ROOT_CLASS,
  PREVIEW_SCHEME_ATTRIBUTE,
  PREVIEW_THEME_SCOPE,
  WCAG_AA_NORMAL_TEXT,
  contrastRatio,
  primaryContrastOf,
  swatchesOf,
} from './preview';
export type { PrimaryContrast, Swatch } from './preview';
