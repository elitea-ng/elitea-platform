/**
 * The connected deployment's brand pack, for the desktop build (ADR-0024
 * channel C, ADR-0029 decision 9).
 *
 * The web app gets its pack from `<script src="/api/v2/branding/bootstrap.js">`
 * in `index.html`, which sets `window.elitea_brand` before the bundle runs.
 * The desktop webview loads its BUNDLED assets, so that relative script tag
 * resolves to the bundle (and its CSP allows no remote script at all): the
 * global stayed unset and every desktop window rendered the compiled-in
 * default pack whatever the deployment's branding said.
 *
 * This module closes the gap without touching the shared brand code:
 *
 *  1. fetch the pack as DATA through the host's network layer —
 *     `GET /api/v2/branding/pack.json` (the native-client form, ADR-0025),
 *     falling back to `bootstrap.js` parsed as JSON, never evaluated, for a
 *     deployment that predates pack.json;
 *  2. validate it with `parseBrandPack`, the exact check channel C runs;
 *  3. inline every asset the pack references on the deployment as a `data:`
 *     URI fetched through the host (the CSP's img-src/font-src admit `data:`,
 *     not the deployment), so logos, favicon and faces render;
 *  4. publish the result as `window.elitea_brand` BEFORE the app is imported,
 *     so `resolveBrandPack()` and every reader of it behave exactly as on the
 *     web, and inject the pack's faces as one `<style>` of `data:` sources
 *     (the shared generator admits same-origin paths only, so the faces are
 *     taken out of the published pack and declared here instead);
 *  5. cache the prepared pack (assets included) per deployment under the
 *     `el.` storage namespace, so the next launch paints branded at once —
 *     the logout sweep and the device wipe both clear it.
 */
import { BRAND_PACK_GLOBAL, type BrandAssetKey, BrandPack, DEFAULT_BRAND_PACK, parseBrandPack } from '@/shared/brand';
import { createStorage } from '@/shared/lib/storage';

/** Storage key (under `el.`) of the cached prepared pack. */
export const BRAND_CACHE_KEY = 'desktop.brand.v1';
/** The attribute of the injected `@font-face` stylesheet. */
export const DESKTOP_FONT_STYLE_ATTRIBUTE = 'data-el-desktop-fonts';

const PACK_JSON_PATH = '/api/v2/branding/pack.json';
const BOOTSTRAP_JS_PATH = '/api/v2/branding/bootstrap.js';
const LAYERS_HEADER = 'x-elitea-brand-layers';
/** The web document base the server resolves `./brand/…` references against (`spaDocumentBase`). */
const DOCUMENT_BASE = '/app/';
const BOOTSTRAP_PREFIX = /^\s*window\.elitea_brand\s*=\s*/;

/** Above the server's per-kind caps (512 KiB image, 300 KiB font), so only a misbehaving answer is refused. */
const MAX_ASSET_BYTES = 768 * 1024;
/** Beyond this the prepared pack is not cached (WebKit's localStorage quota is ~5 MB). */
const MAX_CACHE_CHARS = 3_000_000;

const IMAGE_TYPES = new Set(['image/svg+xml', 'image/png', 'image/webp', 'image/x-icon', 'image/vnd.microsoft.icon']);
const FONT_TYPES = new Set(['font/woff2']);
const DATA_IMAGE_RE = /^data:image\/[a-z0-9.+-]+(?:;[a-z0-9=.+-]+)*[;,]/i;
const DATA_FONT_RE = /^data:font\/woff2;base64,[A-Za-z0-9+/]+=*$/;
const FONT_WEIGHT_RE = /^(?:normal|bold|[1-9]\d{0,2}(?:\s+[1-9]\d{0,2})?)$/;
const ASSET_KEYS: readonly BrandAssetKey[] = ['logoFull', 'logoMark', 'favicon', 'loginArt'];

/** One face of the pack, its source inlined as a `data:font/woff2` URI. */
export interface DesktopFontFace {
  family: string;
  src: string;
  weight?: string;
  style?: 'normal' | 'italic';
}

/** What the desktop applies: `pack: null` means the deployment is unbranded (the compiled default wins, as on the web). */
export interface PreparedBrand {
  pack: BrandPack | null;
  fonts: DesktopFontFace[];
}

export interface CachedBrand extends PreparedBrand {
  origin: string;
  /** The pack.json entity tag the entry was prepared from; `null` when unknown. */
  etag: string | null;
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

/** A deployment pack as fetched: the raw candidate (`null` for "unbranded"), or a 304 against the cached tag. */
interface FetchedPack {
  candidate: unknown;
  etag: string | null;
  notModified: boolean;
}

function trimOrigin(origin: string): string {
  return origin.replace(/\/+$/, '');
}

/**
 * Reads a channel-C payload WITHOUT evaluating it: a JSON object as
 * `pack.json` serves it, or `bootstrap.js`'s `window.elitea_brand = {…};`
 * with the assignment stripped and the remainder parsed as JSON. The server
 * writes that body with `json.Marshal`, so anything that is not exactly one
 * assignment of one JSON object — a second statement, a call, a function —
 * fails `JSON.parse` and is refused.
 *
 * @returns the parsed object; `null` for the inert "no pack configured" body
 *   (a comment and nothing else); `undefined` for anything refused.
 */
export function parseBrandPayload(text: string): unknown {
  const trimmed = text.trim();
  if (trimmed.startsWith('/*') && trimmed.indexOf('*/') === trimmed.length - 2) return null;
  let json = trimmed;
  if (BOOTSTRAP_PREFIX.test(json)) {
    json = json.replace(BOOTSTRAP_PREFIX, '').replace(/;\s*$/, '');
  }
  if (!json.startsWith('{')) return undefined;
  try {
    const value: unknown = JSON.parse(json);
    return value !== null && typeof value === 'object' && !Array.isArray(value) ? value : undefined;
  } catch {
    return undefined;
  }
}

async function fetchPack(fetchImpl: FetchLike, origin: string, etag: string | null): Promise<FetchedPack | undefined> {
  const base = trimOrigin(origin);
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (etag !== null) headers['If-None-Match'] = etag;
  const response = await fetchImpl(`${base}${PACK_JSON_PATH}`, { credentials: 'omit', redirect: 'error', headers });
  if (response.status === 304) return { candidate: undefined, etag, notModified: true };
  if (response.ok) {
    const layers = (response.headers.get(LAYERS_HEADER) ?? '').trim();
    const candidate = parseBrandPayload(await response.text());
    if (candidate === undefined) return undefined;
    // "default" alone = no layer contributed: the server serves the product
    // default for clients with no compiled-in look, and the web app in that
    // state keeps its compiled pack (bootstrap.js leaves the global unset).
    return { candidate: layers === 'default' ? null : candidate, etag: response.headers.get('etag'), notModified: false };
  }
  if (response.status !== 404) return undefined;
  // A deployment from before pack.json: read bootstrap.js as data.
  const legacy = await fetchImpl(`${base}${BOOTSTRAP_JS_PATH}`, { credentials: 'omit', redirect: 'error' });
  if (!legacy.ok) return undefined;
  const candidate = parseBrandPayload(await legacy.text());
  return candidate === undefined ? undefined : { candidate, etag: null, notModified: false };
}

function bytesToBase64(bytes: Uint8Array): string {
  let binary = '';
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

/** The absolute URL a pack reference names, resolved as the web document would; `undefined` when it does not parse. */
function resolveReference(reference: string, origin: string): URL | undefined {
  try {
    return new URL(reference, `${trimOrigin(origin)}${DOCUMENT_BASE}`);
  } catch {
    return undefined;
  }
}

/**
 * Fetches one same-origin asset through the host and returns it as a `data:`
 * URI, or `undefined` when it is foreign, missing, oversized or of a type the
 * slot does not take. The content type is the server's, checked against an
 * allow-list, so a pack cannot turn an asset slot into something else.
 */
async function inlineAsset(fetchImpl: FetchLike, url: URL, origin: string, types: ReadonlySet<string>): Promise<string | undefined> {
  try {
    if (url.origin !== new URL(origin).origin) return undefined;
    const response = await fetchImpl(url.href, { credentials: 'omit', redirect: 'error' });
    if (!response.ok) return undefined;
    const type = (response.headers.get('content-type') ?? '').split(';')[0]?.trim().toLowerCase() ?? '';
    if (!types.has(type)) return undefined;
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.length === 0 || bytes.length > MAX_ASSET_BYTES) return undefined;
    return `data:${type};base64,${bytesToBase64(bytes)}`;
  } catch {
    return undefined;
  }
}

async function prepareAsset(fetchImpl: FetchLike, key: BrandAssetKey, value: string | undefined, origin: string): Promise<string | undefined> {
  const fallback = DEFAULT_BRAND_PACK.assets[key];
  if (value === undefined || value === '') return value;
  if (DATA_IMAGE_RE.test(value)) return value;
  const url = resolveReference(value, origin);
  if (url === undefined) return fallback;
  // The server states the compiled default's own `./brand/…` artwork as an
  // absolute URL; restating the default keeps the slot "not custom", so the
  // compiled SVG renders — exactly what the web app shows for it.
  if (fallback !== undefined) {
    const defaultUrl = resolveReference(fallback, origin);
    if (defaultUrl !== undefined && defaultUrl.href === url.href) return fallback;
  }
  // An asset that cannot be inlined degrades to the default artwork rather
  // than a broken image.
  return (await inlineAsset(fetchImpl, url, origin, IMAGE_TYPES)) ?? fallback;
}

/**
 * Turns a validated deployment pack into what the desktop applies: every
 * asset inlined, every face inlined and moved out of the pack.
 */
async function prepareBrand(fetchImpl: FetchLike, pack: BrandPack, origin: string): Promise<PreparedBrand> {
  const assets = { ...pack.assets };
  await Promise.all(
    ASSET_KEYS.map(async (key) => {
      const value = await prepareAsset(fetchImpl, key, pack.assets[key], origin);
      if (value === undefined) delete assets[key];
      else assets[key] = value;
    }),
  );
  const fonts = (
    await Promise.all(
      (pack.typography.fontFaces ?? []).map(async (face): Promise<DesktopFontFace | undefined> => {
        const url = resolveReference(face.url, origin);
        const src = url === undefined ? undefined : await inlineAsset(fetchImpl, url, origin, FONT_TYPES);
        if (src === undefined) return undefined;
        return { family: face.family, src, ...(face.weight === undefined ? {} : { weight: face.weight }), ...(face.style === undefined ? {} : { style: face.style }) };
      }),
    )
  ).filter((face): face is DesktopFontFace => face !== undefined);
  const { fontFaces: _faces, ...typography } = pack.typography;
  return { pack: { ...pack, assets, typography }, fonts };
}

/**
 * Validates a fetched candidate exactly as channel C does. `parseBrandPack`
 * answers the compiled default (by identity) for anything it refuses, which
 * is how a refusal is told apart here.
 */
function validateBrandCandidate(candidate: unknown): BrandPack | undefined {
  if (candidate === null || candidate === undefined) return undefined;
  const pack = parseBrandPack(candidate);
  return pack === DEFAULT_BRAND_PACK ? undefined : pack;
}

/* ── cache ───────────────────────────────────────────────────────────────── */

function isFontFace(value: unknown): value is DesktopFontFace {
  if (value === null || typeof value !== 'object') return false;
  const face = value as Record<string, unknown>;
  return typeof face.family === 'string' && typeof face.src === 'string' && DATA_FONT_RE.test(face.src);
}

function validateCached(raw: unknown): CachedBrand | undefined {
  if (raw === null || typeof raw !== 'object') return undefined;
  const entry = raw as Record<string, unknown>;
  if (typeof entry.origin !== 'string' || !Array.isArray(entry.fonts) || !entry.fonts.every(isFontFace)) return undefined;
  const etag = typeof entry.etag === 'string' ? entry.etag : null;
  if (entry.pack === null) return { origin: entry.origin, etag, pack: null, fonts: [] };
  const parsed = BrandPack.safeParse(entry.pack);
  return parsed.success ? { origin: entry.origin, etag, pack: parsed.data, fonts: entry.fonts } : undefined;
}

/** The cached prepared pack for `origin`, or `undefined` (none, another deployment's, or corrupt). */
export function readCachedBrand(origin: string): CachedBrand | undefined {
  try {
    const cached = createStorage('local').getJSON(BRAND_CACHE_KEY, validateCached) ?? undefined;
    return cached?.origin === trimOrigin(origin) ? cached : undefined;
  } catch {
    return undefined;
  }
}

/** Stores the prepared pack for `origin`; a pack too large for the quota is not cached (the next launch fetches it). */
export function writeCachedBrand(entry: CachedBrand): void {
  try {
    const storage = createStorage('local');
    const value = JSON.stringify({ ...entry, origin: trimOrigin(entry.origin) });
    if (value.length > MAX_CACHE_CHARS) {
      storage.remove(BRAND_CACHE_KEY);
      return;
    }
    storage.set(BRAND_CACHE_KEY, value);
  } catch {
    // Storage unavailable or over quota: the brand still applies this launch.
  }
}

/* ── apply ───────────────────────────────────────────────────────────────── */

function cssString(value: string): string {
  return `"${value.replace(/["\\]/g, '').replace(/\s+/g, ' ').trim()}"`;
}

/** The `@font-face` stylesheet for inlined faces; a face whose source is not a `data:font/woff2` URI is dropped. */
export function desktopFontStylesheet(fonts: readonly DesktopFontFace[]): string {
  return fonts
    .filter(isFontFace)
    .map((face) => {
      const declarations = [`font-family:${cssString(face.family)}`, `src:url("${face.src}") format("woff2")`, 'font-display:swap'];
      if (face.weight !== undefined && FONT_WEIGHT_RE.test(face.weight.trim())) declarations.push(`font-weight:${face.weight.trim()}`);
      if (face.style === 'normal' || face.style === 'italic') declarations.push(`font-style:${face.style}`);
      return `@font-face{${declarations.join(';')};}`;
    })
    .join('\n');
}

/**
 * Publishes a prepared pack where the shared brand code reads it. Must run
 * before the app module is imported: the app resolves its pack once, at
 * mount. An unbranded deployment leaves the global unset, as the web does.
 */
export function applyDesktopBrand(brand: PreparedBrand, doc: Document = document): void {
  const globals = globalThis as unknown as Record<string, unknown>;
  if (brand.pack === null) delete globals[BRAND_PACK_GLOBAL];
  else globals[BRAND_PACK_GLOBAL] = brand.pack;

  const css = desktopFontStylesheet(brand.fonts);
  const existing = doc.head.querySelector<HTMLStyleElement>(`style[${DESKTOP_FONT_STYLE_ATTRIBUTE}]`);
  if (css === '') {
    existing?.remove();
    return;
  }
  const style = existing ?? doc.createElement('style');
  style.setAttribute(DESKTOP_FONT_STYLE_ATTRIBUTE, '');
  style.textContent = css;
  if (existing === null) doc.head.append(style);
}

/* ── orchestration ───────────────────────────────────────────────────────── */

/**
 * Fetches, validates and prepares the deployment's pack, and caches the
 * result. Resolves `undefined` when nothing usable came back (network error,
 * refused payload) — the caller then keeps what it had. A 304 against the
 * cached entry's tag resolves the cached entry unchanged.
 */
export async function refreshBrand(fetchImpl: FetchLike, origin: string, cached?: CachedBrand): Promise<CachedBrand | undefined> {
  try {
    const fetched = await fetchPack(fetchImpl, origin, cached?.etag ?? null);
    if (fetched === undefined) return undefined;
    if (fetched.notModified) return cached;
    const pack = validateBrandCandidate(fetched.candidate);
    // Unbranded, or a refused pack: the compiled default, as channel C does.
    const prepared: PreparedBrand = pack === undefined ? { pack: null, fonts: [] } : await prepareBrand(fetchImpl, pack, origin);
    const entry: CachedBrand = { ...prepared, origin: trimOrigin(origin), etag: fetched.etag };
    writeCachedBrand(entry);
    return entry;
  } catch {
    // Branding never blocks the app: the caller keeps what it had.
    return undefined;
  }
}

export interface LoadDesktopBrandOptions {
  fetch: FetchLike;
  origin: string;
  /** How long a launch with no cached pack waits for the deployment's before mounting with the default. */
  firstLoadTimeoutMs?: number;
}

/**
 * Applies the deployment's brand for this launch:
 *  - a cached pack is applied at once and refreshed in the background (a
 *    change shows from the next launch: the app holds its pack for the
 *    document's lifetime, as on the web);
 *  - with none, the pack is fetched and applied, waiting at most
 *    `firstLoadTimeoutMs`; a slower answer is still cached for next time.
 *
 * @returns the background refresh, for tests and callers that want to await it.
 */
export async function loadDesktopBrand(options: LoadDesktopBrandOptions): Promise<{ refreshed: Promise<CachedBrand | undefined> }> {
  const { fetch: fetchImpl, origin, firstLoadTimeoutMs = 5000 } = options;
  const cached = readCachedBrand(origin);
  const refreshed = refreshBrand(fetchImpl, origin, cached);
  if (cached !== undefined) {
    applyDesktopBrand(cached);
    return { refreshed };
  }
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<undefined>((resolve) => {
    timer = setTimeout(() => resolve(undefined), firstLoadTimeoutMs);
  });
  const first = await Promise.race([refreshed, timeout]);
  clearTimeout(timer);
  if (first !== undefined) applyDesktopBrand(first);
  return { refreshed };
}
