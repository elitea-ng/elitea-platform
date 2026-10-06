import { z } from 'zod';

import { anonymousFetch, readJson, type AnonymousFetchOptions } from '@/api/anonymous';
import { BRAND_PACK_PATH } from '@/api/discovery';
import { isSameOrigin, originOf } from '@/api/paths';
import type { Palette } from '@/constants/theme';
import { log } from '@/lib/log';
import { sha256Hex } from '@/lib/sha256';
import { BLACK, WHITE, contrastRatio, formatColor, parseColor, type Rgba } from '@/theme/color';

/**
 * The workspace brand pack (ADR-0025 WP1, `GET /api/v2/branding/pack.json`)
 * mapped to the app's theme tokens.
 *
 * Rules:
 *  - The pack URL must be the discovery document's, on the workspace origin.
 *  - Only an allowlist of tokens is read (`TOKEN_MAP`), and only plain colours
 *    are accepted (`parseColor`); a scheme whose text does not reach WCAG
 *    4.5:1 on its background is dropped whole, weaker tokens individually.
 *  - Images and fonts are loaded ONLY from the workspace origin (or, for
 *    raster images, an inline `data:` URI). Anything else is ignored: the app
 *    never fetches a brand asset from a third party.
 *  - Web font stacks are ignored (the app renders with system fonts) unless
 *    the pack ships a same-origin TrueType/OpenType face for its first
 *    family, which is then loaded from the workspace.
 */

const TOKEN = z.string().max(256);
const SchemeSchema = z.record(z.string().max(128), TOKEN);

export const BrandPackSchema = z.looseObject({
  product: z.looseObject({ name: z.string().max(200), shortName: z.string().max(100) }),
  assets: z.looseObject({
    logoFull: z.string().max(400_000),
    logoMark: z.string().max(400_000),
  }),
  typography: z
    .looseObject({
      fontFamily: z.string().max(400).optional(),
      fontFaces: z
        .array(z.looseObject({ family: z.string().max(100), url: z.string().max(2048) }))
        .max(32)
        .optional(),
    })
    .optional(),
  brand: z.looseObject({ hue: TOKEN }),
  schemes: z.looseObject({ light: SchemeSchema, dark: SchemeSchema }),
});

export type BrandPack = z.infer<typeof BrandPackSchema>;

export type ColorScheme = 'light' | 'dark';
export type PaletteKey = keyof Palette;

/**
 * App token ← pack token ids, first match wins. The ids are the web app's
 * token vocabulary (in it `text.secondary` is the strong text colour and
 * `text.primary` the muted one).
 */
export const TOKEN_MAP: Record<PaletteKey, readonly string[]> = {
  text: ['text.secondary'],
  textSecondary: ['text.primary'],
  background: ['background.default'],
  backgroundElement: ['background.secondary'],
  border: ['border.lines'],
  accent: ['primary.main'],
  onAccent: ['text.button.primary'],
  danger: ['error.main'],
};

/** Minimum contrast against the scheme background, per token. */
const MIN_CONTRAST: Partial<Record<PaletteKey, number>> = {
  text: 4.5,
  textSecondary: 3,
  accent: 3,
  danger: 3,
};

export interface BrandImage {
  uri: string;
  kind: 'svg' | 'raster';
}

export interface BrandFontFace {
  /** Name the face is registered under in the app (namespaced). */
  alias: string;
  url: string;
}

export interface BrandTheme {
  origin: string;
  productName: string;
  /** The server served its product default: the deployment has no brand of its own. */
  productDefault: boolean;
  logoMark: BrandImage | null;
  logoFull: BrandImage | null;
  light: Partial<Palette>;
  dark: Partial<Palette>;
  /** Same-origin faces to load; `fontFamily` is the alias to render with once loaded. */
  fontFaces: BrandFontFace[];
  fontFamily: string | null;
  /**
   * The pack's accent inputs as served, before `mapScheme`'s contrast filter and
   * hue fill. The design system (`src/design/brand.ts`) corrects a weak accent
   * instead of dropping it and derives a missing scheme from the other one, so
   * it needs these. Optional: themes persisted before it existed lack it.
   */
  accentSource?: BrandAccentSource;
}

/** Plain colours (`formatColor` form) or null; never contrast-filtered. */
export interface BrandAccentSource {
  hue: string | null;
  lightAccent: string | null;
  darkAccent: string | null;
  lightOn: string | null;
  darkOn: string | null;
}

const RASTER_DATA_URI = /^data:image\/(png|jpeg|webp);base64,[A-Za-z0-9+/=]+$/;
const SAFE_PATH = /^\/[A-Za-z0-9._~\-/%]*$/;

function pathOf(url: string): string | null {
  const origin = originOf(url);
  if (origin === null) return null;
  const rest = url.slice(origin.length);
  const path = rest.split(/[?#]/, 1)[0] || '/';
  if (!SAFE_PATH.test(path) || path.split('/').some((s) => s === '..' || s === '.')) return null;
  return path;
}

/** A brand image the app may load: a same-origin URL, or an inline raster data URI. */
export function acceptImage(value: string, origin: string): BrandImage | null {
  if (RASTER_DATA_URI.test(value)) return { uri: value, kind: 'raster' };
  if (!isSameOrigin(value, origin)) return null;
  const path = pathOf(value);
  if (path === null) return null;
  const lower = path.toLowerCase();
  if (lower.endsWith('.svg')) return { uri: value, kind: 'svg' };
  if (/\.(png|jpe?g|webp)$/.test(lower)) return { uri: value, kind: 'raster' };
  return null;
}

/** First family of a CSS font stack, unquoted. */
export function firstFamily(stack: string | undefined): string | null {
  if (!stack) return null;
  const first = stack.split(',')[0]?.trim().replace(/^["']|["']$/g, '');
  return first && /^[A-Za-z0-9 _-]{1,64}$/.test(first) ? first : null;
}

function mapFonts(pack: BrandPack, origin: string): Pick<BrandTheme, 'fontFaces' | 'fontFamily'> {
  const family = firstFamily(pack.typography?.fontFamily);
  if (family === null) return { fontFaces: [], fontFamily: null };
  const faces = (pack.typography?.fontFaces ?? [])
    .filter((face) => face.family === family && isSameOrigin(face.url, origin))
    .filter((face) => /\.(ttf|otf)$/i.test(pathOf(face.url) ?? ''))
    .slice(0, 1);
  if (faces.length === 0) return { fontFaces: [], fontFamily: null };
  const alias = brandFontAlias(origin, faces[0].url);
  return { fontFaces: faces.map((face) => ({ alias, url: face.url })), fontFamily: alias };
}

/**
 * The name a workspace's brand face is registered (and cached) under. Fonts
 * are registered process-wide by name and the loader skips a name it already
 * loaded, so the name must never be shared by two workspaces: it is 128 bits
 * of SHA-256 over the workspace origin and the face URL. A hostile workspace
 * can read another's (public) brand pack, so a short or non-cryptographic hash
 * (32-bit FNV-1a, before) could be collided on purpose in seconds to make the
 * other workspace render with its font. The family name is not part of it:
 * it is chosen by the pack and carries nothing the digest does not.
 */
export function brandFontAlias(origin: string, faceUrl: string): string {
  return `brand-${sha256Hex(`${origin}\n${faceUrl}`).slice(0, 32)}`;
}

/** The allowlisted, contrast-checked palette overrides of one scheme. */
export function mapScheme(tokens: Record<string, string>, hue: string): Partial<Palette> {
  const pick = (key: PaletteKey): Rgba | null => {
    for (const id of TOKEN_MAP[key]) {
      const color = parseColor(tokens[id]);
      if (color) return color;
    }
    return key === 'accent' ? parseColor(hue) : null;
  };

  const background = pick('background');
  const text = pick('text');
  // Without an opaque background and readable text the scheme is unusable.
  if (!background || background.a < 1 || !text || contrastRatio(text, background) < 4.5) return {};

  const out: Partial<Palette> = { background: formatColor(background), text: formatColor(text) };
  for (const key of ['textSecondary', 'accent', 'danger'] as const) {
    const color = pick(key);
    if (color && contrastRatio(color, background) >= (MIN_CONTRAST[key] ?? 1)) out[key] = formatColor(color);
  }
  const element = pick('backgroundElement');
  if (element && element.a === 1 && contrastRatio(text, element) >= 4.5) out.backgroundElement = formatColor(element);
  const border = pick('border');
  if (border) out.border = formatColor(border);

  const accent = out.accent ? parseColor(out.accent) : null;
  if (accent) {
    const onAccent = pick('onAccent');
    const solidAccent = accent.a < 1 ? null : accent;
    if (solidAccent) {
      out.onAccent =
        onAccent && contrastRatio(onAccent, solidAccent) >= 4.5
          ? formatColor(onAccent)
          : formatColor(contrastRatio(WHITE, solidAccent) >= contrastRatio(BLACK, solidAccent) ? WHITE : BLACK);
    } else {
      delete out.accent; // a translucent accent cannot carry button text
    }
  }
  return out;
}

/** First plain colour among the pack token ids `TOKEN_MAP[key]`, in `formatColor` form. */
function rawToken(tokens: Record<string, string> | undefined, key: PaletteKey): string | null {
  for (const id of TOKEN_MAP[key]) {
    const color = parseColor(tokens?.[id]);
    if (color) return formatColor(color);
  }
  return null;
}

function plain(value: string | null | undefined): string | null {
  const color = parseColor(value ?? undefined);
  return color ? formatColor(color) : null;
}

/** The unfiltered accent inputs of a pack (see `BrandTheme.accentSource`). */
export function accentSourceOf(schemes: { light?: Record<string, string>; dark?: Record<string, string> }, hue: string | null): BrandAccentSource {
  return {
    hue: plain(hue),
    lightAccent: rawToken(schemes.light, 'accent'),
    darkAccent: rawToken(schemes.dark, 'accent'),
    lightOn: rawToken(schemes.light, 'onAccent'),
    darkOn: rawToken(schemes.dark, 'onAccent'),
  };
}

/** Re-validates a persisted accent source: anything that is not a plain colour becomes null. */
export function cleanAccentSource(value: Partial<Record<keyof BrandAccentSource, string | null>>): BrandAccentSource {
  return {
    hue: plain(value.hue),
    lightAccent: plain(value.lightAccent),
    darkAccent: plain(value.darkAccent),
    lightOn: plain(value.lightOn),
    darkOn: plain(value.darkOn),
  };
}

export function mapBrandPack(pack: BrandPack, origin: string, layers: string | null): BrandTheme {
  return {
    origin,
    productName: pack.product.name.trim().slice(0, 80),
    productDefault: (layers ?? '').split(',').map((l) => l.trim()).join(',') === 'default',
    logoMark: acceptImage(pack.assets.logoMark, origin),
    logoFull: acceptImage(pack.assets.logoFull, origin),
    light: mapScheme(pack.schemes.light, pack.brand.hue),
    dark: mapScheme(pack.schemes.dark, pack.brand.hue),
    ...mapFonts(pack, origin),
    accentSource: accentSourceOf(pack.schemes, pack.brand.hue),
  };
}

/** Whether `url` is the workspace's brand pack route. */
export function isBrandPackUrl(url: string, origin: string): boolean {
  return isSameOrigin(url, origin) && pathOf(url) === BRAND_PACK_PATH;
}

/** Fetches and maps the pack. Returns null on any failure (the app keeps its fallback theme). */
export async function fetchBrandTheme(
  origin: string,
  packUrl: string,
  options: AnonymousFetchOptions = {},
): Promise<BrandTheme | null> {
  if (!isBrandPackUrl(packUrl, origin)) {
    log.warn('brand pack URL refused (not the workspace pack route)');
    return null;
  }
  try {
    const response = await anonymousFetch(origin, packUrl, {}, options);
    if (response.status !== 200) return null;
    const parsed = BrandPackSchema.safeParse(await readJson(response));
    if (!parsed.success) {
      log.warn('brand pack rejected', { issues: parsed.error.issues.length });
      return null;
    }
    return mapBrandPack(parsed.data, origin, response.headers.get('X-Elitea-Brand-Layers'));
  } catch (error) {
    log.info('brand pack unavailable', { error });
    return null;
  }
}
