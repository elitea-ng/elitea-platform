/**
 * The `branding` section's value model, as the admin Branding page edits it
 * (ADR-0024 WP4).
 *
 * `GET /admin/branding/administration` answers `values` as an open record: the
 * section's declared keys with stored values overlaid on the schema defaults,
 * where an EMPTY STRING or 0 means "inherit from the layer below" (the mounted
 * file pack, else the product default). This module gives that record a type,
 * a parser, and the two pure derivations the page is built on:
 *
 *  - `applyDraftToPack` mirrors the Go overlay (`internal/api/v2/branding/
 *    resolver.go`'s `applyOverlay`) field for field, so the preview theme is
 *    built from the pack the bootstrap route WOULD serve after a save.
 *  - `brandingErrorField` reads the field a 400 names, so the server's reason
 *    lands beside the input it is about rather than only in a banner.
 *
 * Nothing here validates. The server owns the rules (six-digit hex, absolute
 * links, same-origin asset paths, at most two font faces) and states each
 * refusal with the key it applies to; a second copy of those rules here would
 * be free to drift.
 */
import { BrandPack, DEFAULT_BRAND_PACK } from '@/shared/brand';
import { parseColor } from '@/shared/brand/color';

const BRANDING_TEXT_KEYS = [
  'product_name',
  'product_short_name',
  'product_tagline',
  'docs_url',
  'support_url',
  'brand_hue',
  'brand_on_brand',
  'font_family',
  'font_family_mono',
  'density',
  'logo_full',
  'logo_mark',
  'favicon',
  'login_art',
  'logo_email',
] as const;

const BRANDING_NUMBER_KEYS = [
  'base_size',
  'scale',
  'radius_sm',
  'radius_md',
  'radius_lg',
  'radius_pill',
] as const;

const BRANDING_FONT_FACES_KEY = 'font_faces';

export type BrandingTextKey = (typeof BRANDING_TEXT_KEYS)[number];
export type BrandingNumberKey = (typeof BRANDING_NUMBER_KEYS)[number];
export type BrandingKey = BrandingTextKey | BrandingNumberKey | typeof BRANDING_FONT_FACES_KEY;

/** Every key the section declares, in the server's order. */
export const BRANDING_KEYS: readonly BrandingKey[] = [
  ...BRANDING_TEXT_KEYS,
  ...BRANDING_NUMBER_KEYS,
  BRANDING_FONT_FACES_KEY,
];

/** One self-hosted face. `url` is the path an upload to `assets/font` returned. */
export interface BrandingFontFace {
  readonly family: string;
  readonly url: string;
  readonly weight?: string;
  readonly style?: string;
}

/** The section's values, typed. Empty string / 0 / [] = inherit. */
export type BrandingValues = Readonly<Record<BrandingTextKey, string>> &
  Readonly<Record<BrandingNumberKey, number>> & {
    readonly font_faces: readonly BrandingFontFace[];
  };

/** The asset-path fields, each fed by one upload kind. */
const BRANDING_ASSET_KEYS = ['logo_full', 'logo_mark', 'favicon', 'login_art', 'logo_email'] as const;
export type BrandingAssetKey = (typeof BRANDING_ASSET_KEYS)[number];

/** Which layer a field's EFFECTIVE value comes from. */
export type BrandingFieldSource = 'database' | 'file' | 'default';

export interface BrandingLayers {
  readonly file: boolean;
  readonly database: boolean;
}

function readString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function readNumber(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function readFontFace(entry: unknown): BrandingFontFace | undefined {
  if (typeof entry !== 'object' || entry === null) return undefined;
  const record = entry as Record<string, unknown>;
  const face: { family: string; url: string; weight?: string; style?: string } = {
    family: readString(record['family']),
    url: readString(record['url']),
  };
  if (typeof record['weight'] === 'string' && record['weight'] !== '') face.weight = record['weight'];
  if (typeof record['style'] === 'string' && record['style'] !== '') face.style = record['style'];
  return face;
}

/** The server's open `values` record, typed; anything malformed reads as "inherit". */
export function parseBrandingValues(raw: unknown): BrandingValues {
  const record = typeof raw === 'object' && raw !== null ? (raw as Record<string, unknown>) : {};
  const text = Object.fromEntries(
    BRANDING_TEXT_KEYS.map((key) => [key, readString(record[key])]),
  ) as Record<BrandingTextKey, string>;
  const numbers = Object.fromEntries(
    BRANDING_NUMBER_KEYS.map((key) => [key, readNumber(record[key])]),
  ) as Record<BrandingNumberKey, number>;
  const faces = record[BRANDING_FONT_FACES_KEY];
  const font_faces = Array.isArray(faces)
    ? faces.map(readFontFace).filter((face): face is BrandingFontFace => face !== undefined)
    : [];
  return { ...text, ...numbers, font_faces };
}

/** Every key this page edits, at "inherit". */
export function emptyBrandingValues(): BrandingValues {
  return parseBrandingValues({});
}

/**
 * The section keys this page does not edit, at "inherit": the hand-tuned
 * scheme tokens a brand-package import writes, and the e-mail sender fields.
 * A save leaves them alone; a reset must not, because the server upserts only
 * the keys it is sent — omitting `scheme_tokens` kept every imported colour.
 */
const UNEDITED_KEYS_INHERIT: Readonly<Record<string, unknown>> = {
  scheme_tokens: {},
  sender_name: '',
  support_email: '',
};

/** The inherit value of a stored value's own shape. */
function inheritLike(value: unknown): unknown {
  if (Array.isArray(value)) return [];
  if (typeof value === 'number') return 0;
  if (typeof value === 'object' && value !== null) return {};
  return '';
}

/**
 * What "Reset to defaults" writes: EVERY key the section stores at inherit —
 * the edited ones, the known unedited ones, and any other key the server
 * answered with (a key added server-side later must not survive a reset).
 */
export function resetBrandingPayload(stored?: unknown): Record<string, unknown> {
  const payload: Record<string, unknown> = {};
  if (typeof stored === 'object' && stored !== null && !Array.isArray(stored)) {
    for (const [key, value] of Object.entries(stored)) payload[key] = inheritLike(value);
  }
  return { ...payload, ...UNEDITED_KEYS_INHERIT, ...emptyBrandingValues() };
}

/** Whether `key` is left to the layer below. */
export function isInherited(values: BrandingValues, key: BrandingKey): boolean {
  if (key === BRANDING_FONT_FACES_KEY) return values.font_faces.length === 0;
  const value = values[key];
  return typeof value === 'number' ? value === 0 : value.trim() === '';
}

/**
 * Where `key`'s effective value comes from. A stored (non-inherit) value is the
 * database layer; an inherited one is the mounted file when one contributes,
 * else the product default. The file's own per-field content is not visible
 * here — a mounted pack is a whole pack — so "file" means "the file decides".
 */
export function brandingFieldSource(
  values: BrandingValues,
  key: BrandingKey,
  layers: BrandingLayers,
): BrandingFieldSource {
  if (!isInherited(values, key)) return 'database';
  return layers.file ? 'file' : 'default';
}

/**
 * The pack the draft is laid over: the served `effective` pack when the server
 * gave one that parses, else the compiled product default — the same fallback
 * `channelC.ts` makes for the running app.
 */
export function basePackFrom(effective: unknown): BrandPack {
  if (effective === null || effective === undefined) return DEFAULT_BRAND_PACK;
  const parsed = BrandPack.safeParse(effective);
  return parsed.success ? parsed.data : DEFAULT_BRAND_PACK;
}

/**
 * The self-hosted faces the served pack carries. They ride in
 * `typography.fontFaces`; the zod schema declares them (ADR-0024 WP3), but
 * they are still read off the RAW document rather than the parsed pack, so a
 * served pack that fails the parse for another reason does not also lose
 * the faces the server stated when the page falls back to the default.
 */
export function effectiveFontFaces(effective: unknown): readonly BrandingFontFace[] {
  if (typeof effective !== 'object' || effective === null) return [];
  const typography = (effective as Record<string, unknown>)['typography'];
  if (typeof typography !== 'object' || typography === null) return [];
  const faces = (typography as Record<string, unknown>)['fontFaces'];
  if (!Array.isArray(faces)) return [];
  return faces.map(readFontFace).filter((face): face is BrandingFontFace => face !== undefined);
}

/**
 * The e-mail logo the served pack carries (`assets.logoEmail`, ADR-0024 WP7).
 * The UI's pack schema has no field for it — no rendered surface uses a raster
 * mail logo — so, like the font faces, it is read off the raw document.
 */
export function effectiveLogoEmail(effective: unknown): string {
  if (typeof effective !== 'object' || effective === null) return '';
  const assets = (effective as Record<string, unknown>)['assets'];
  if (typeof assets !== 'object' || assets === null) return '';
  return readString((assets as Record<string, unknown>)['logoEmail']);
}

const text = (value: string): string | undefined => (value.trim() === '' ? undefined : value);
const number = (value: number): number | undefined => (value === 0 ? undefined : value);
/** A colour the theme builder can parse; anything else keeps the base (mid-typing). */
const colour = (value: string): string | undefined =>
  parseColor(value) === null ? undefined : value;

/** `{...target, [key]: value}` only when `value` is set — `exactOptionalPropertyTypes` forbids an explicit undefined. */
function withOptional<T extends object, K extends string>(
  target: T,
  key: K,
  value: string | undefined,
): T & Partial<Record<K, string>> {
  return value === undefined ? target : { ...target, [key]: value };
}

/**
 * The Go overlay, field for field: a set value replaces the base, an empty
 * string or 0 keeps it (`applyOverlay` in resolver.go). One deliberate
 * difference: a colour the builder cannot parse keeps the base instead of
 * being applied verbatim, so the preview can still build while the operator is
 * halfway through typing a hex; the server refuses such a value on save.
 *
 * `font_faces` is not applied: the UI's pack schema has no field for it (the
 * faces are declared as `@font-face` by the bootstrap route, not by the
 * theme), so a preview cannot load them. `fontFamily` is applied, which is
 * what a face is FOR.
 */
export function applyDraftToPack(pack: BrandPack, values: BrandingValues): BrandPack {
  return {
    ...pack,
    product: productFrom(pack, values),
    brand: brandFrom(pack, values),
    assets: assetsFrom(pack, values),
    typography: typographyFrom(pack, values),
    shape: shapeFrom(pack, values),
  };
}

function productFrom(pack: BrandPack, values: BrandingValues): BrandPack['product'] {
  const named = {
    ...pack.product,
    name: text(values.product_name) ?? pack.product.name,
    shortName: text(values.product_short_name) ?? pack.product.shortName,
  };
  const withTagline = withOptional(named, 'tagline', text(values.product_tagline));
  const withDocs = withOptional(withTagline, 'docsUrl', text(values.docs_url));
  return withOptional(withDocs, 'supportUrl', text(values.support_url));
}

function brandFrom(pack: BrandPack, values: BrandingValues): BrandPack['brand'] {
  return withOptional(
    { ...pack.brand, hue: colour(values.brand_hue) ?? pack.brand.hue },
    'onBrand',
    colour(values.brand_on_brand),
  );
}

function assetsFrom(pack: BrandPack, values: BrandingValues): BrandPack['assets'] {
  return withOptional(
    {
      ...pack.assets,
      logoFull: text(values.logo_full) ?? pack.assets.logoFull,
      logoMark: text(values.logo_mark) ?? pack.assets.logoMark,
      favicon: text(values.favicon) ?? pack.assets.favicon,
    },
    'loginArt',
    text(values.login_art),
  );
}

function typographyFrom(pack: BrandPack, values: BrandingValues): BrandPack['typography'] {
  return {
    ...pack.typography,
    fontFamily: text(values.font_family) ?? pack.typography.fontFamily,
    fontFamilyMono: text(values.font_family_mono) ?? pack.typography.fontFamilyMono,
    baseSize: number(values.base_size) ?? pack.typography.baseSize,
    scale: number(values.scale) ?? pack.typography.scale,
  };
}

function shapeFrom(pack: BrandPack, values: BrandingValues): BrandPack['shape'] {
  const density =
    values.density === 'compact' || values.density === 'comfortable'
      ? values.density
      : pack.shape.density;
  return {
    ...pack.shape,
    radiusSm: number(values.radius_sm) ?? pack.shape.radiusSm,
    radiusMd: number(values.radius_md) ?? pack.shape.radiusMd,
    radiusLg: number(values.radius_lg) ?? pack.shape.radiusLg,
    radiusPill: number(values.radius_pill) ?? pack.shape.radiusPill,
    density,
  };
}

/**
 * A stated scheme token SHADOWS derivation (`toMuiPalette.resolveScheme`:
 * stated wins, derived fills the rest), and the product default states every
 * token. Laid over that base, a new hue would derive nothing and the preview
 * would not move. When the draft's hue differs from the base's, the stated
 * records are dropped so every token derives from the hue — the shape a
 * hue-only tenant pack has, and what the swatch strip exists to show.
 */
export function withDerivedSchemes(pack: BrandPack, baseHue: string): BrandPack {
  if (pack.brand.hue === baseHue) return pack;
  return { ...pack, schemes: { light: {}, dark: {} } };
}

/**
 * The key a server refusal names, when it names one this section declares.
 *
 * Refusals are `"<key>" must be …` (Go's `%q`), and one of them —
 * `unknown configuration key for section "branding": "<key>"` — quotes the
 * section id first, which is why the first quoted token that IS a declared
 * key wins rather than the first quoted token.
 */
export function brandingErrorField(message: string): BrandingKey | undefined {
  const declared = new Set<string>(BRANDING_KEYS);
  for (const match of message.matchAll(/"([^"]+)"/g)) {
    const candidate = match[1];
    if (candidate !== undefined && declared.has(candidate)) return candidate as BrandingKey;
  }
  return undefined;
}

/** What the server accepts as a brand colour: `#` and six hex digits. */
export function isSixDigitHex(value: string): boolean {
  return /^#[0-9a-fA-F]{6}$/.test(value.trim());
}
