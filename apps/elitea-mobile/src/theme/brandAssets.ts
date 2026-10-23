import { Directory, File, Paths } from 'expo-file-system';

import { anonymousFetch } from '@/api/anonymous';
import type { BaseFetch } from '@/api/middleware';
import { noRedirectFetch } from '@/api/noRedirectFetch';
import { originOf } from '@/api/paths';
import { log } from '@/lib/log';
import { sha256Hex } from '@/lib/sha256';
import { registerStoredWipeHook } from '@/security/endSession';
import { SECURE_KEYS, getSecureItemIn } from '@/security/secureStorage';
import type { BrandFontFace } from '@/theme/brandPack';

/**
 * Raster logos and font faces of the brand pack, downloaded so that nothing
 * outside the workspace origin is ever fetched or parsed because of them
 * (SEC-3).
 *
 * `<Image source={{ uri }}>` and expo-font's `loadAsync({ uri })` download
 * with the platform loaders, which follow a redirect to any host: a
 * same-origin pack URL that redirected (an open redirect, a misconfigured
 * asset path) made the app request a third-party host before sign-in, and
 * handed third-party bytes to the native font parser. Instead each asset is
 * fetched through the origin-guarded anonymous fetch with redirects refused,
 * its type and size are checked, and only those bytes reach the image view
 * (as a `data:` URI) or the font loader (as a file in the caches directory,
 * which the session wipe empties).
 *
 * Font files live in a folder of their workspace's origin
 * (`<caches>/brand-fonts/<digest of origin>/`), deleted with the workspace
 * even when it is not the active one (removing it from the switcher, a wipe
 * resumed at launch): the files name its brand and server.
 */

export const BRAND_FONT_DIR = 'brand-fonts';

function fontDirectory(origin: string): Directory {
  return new Directory(Paths.cache, BRAND_FONT_DIR, sha256Hex(origin).slice(0, 32));
}

/** Deletes the brand font files downloaded for `origin`. Throws when they could not be deleted. */
export function deleteBrandFonts(origin: string): void {
  const directory = fontDirectory(origin);
  if (directory.exists) directory.delete();
}

export const MAX_RASTER_BYTES = 512 * 1024;
export const MAX_FONT_BYTES = 2 * 1024 * 1024;

const RASTER_TYPES: Record<string, string> = { png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', webp: 'image/webp' };
const FONT_TYPES = /^(font\/(ttf|otf|sfnt)|application\/(x-font-ttf|x-font-otf|font-sfnt|vnd\.ms-opentype|octet-stream))$/;

const extensionOf = (uri: string) => (uri.split(/[?#]/, 1)[0].match(/\.([a-z0-9]+)$/i)?.[1] ?? '').toLowerCase();

/**
 * The asset's bytes when it is answered 200 by its own origin without a
 * redirect, with a Content-Type matching `type` and at most `maxBytes` long;
 * otherwise null.
 */
export async function fetchBrandAsset(
  uri: string,
  type: RegExp,
  maxBytes: number,
  fetchImpl: BaseFetch = noRedirectFetch,
): Promise<Uint8Array | null> {
  const origin = originOf(uri);
  if (origin === null) return null;
  try {
    const response = await anonymousFetch(origin, uri, { headers: { Accept: '*/*' } }, { fetch: fetchImpl });
    if (response.status !== 200) return null;
    const contentType = (response.headers.get('Content-Type') ?? '').split(';', 1)[0].trim().toLowerCase();
    if (!type.test(contentType)) return null;
    const declared = Number(response.headers.get('Content-Length') ?? NaN);
    if (Number.isFinite(declared) && declared > maxBytes) return null;
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength === 0 || bytes.byteLength > maxBytes) return null;
    return bytes;
  } catch (error) {
    log.info('brand asset unavailable', { error });
    return null;
  }
}

/** A same-origin raster logo as a `data:` URI of its vetted type, or null. */
export async function fetchBrandRaster(uri: string, fetchImpl: BaseFetch = noRedirectFetch): Promise<string | null> {
  const mime = RASTER_TYPES[extensionOf(uri)];
  if (mime === undefined) return null;
  const bytes = await fetchBrandAsset(uri, new RegExp(`^${mime.replace('/', '\\/')}$`), MAX_RASTER_BYTES, fetchImpl);
  return bytes === null ? null : `data:${mime};base64,${base64Of(bytes)}`;
}

/** Downloads a vetted font face to the caches directory; its local file URI, or null. */
export async function downloadBrandFont(face: BrandFontFace, fetchImpl: BaseFetch = noRedirectFetch): Promise<string | null> {
  const extension = extensionOf(face.url);
  if (extension !== 'ttf' && extension !== 'otf') return null;
  const bytes = await fetchBrandAsset(face.url, FONT_TYPES, MAX_FONT_BYTES, fetchImpl);
  if (bytes === null) return null;
  const origin = originOf(face.url);
  if (origin === null) return null;
  try {
    const directory = fontDirectory(origin);
    if (!directory.exists) directory.create({ intermediates: true });
    const file = new File(directory, `${face.alias}.${extension}`);
    if (file.exists) file.delete();
    file.create();
    file.write(bytes);
    return file.uri;
  } catch (error) {
    log.info('brand font not stored', { error });
    return null;
  }
}

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

/** Standard base64 (RFC 4648, padded), without relying on a global btoa. */
export function base64Of(bytes: Uint8Array): string {
  let out = '';
  let i = 0;
  for (; i + 2 < bytes.length; i += 3) {
    const n = (bytes[i] << 16) | (bytes[i + 1] << 8) | bytes[i + 2];
    out += ALPHABET[(n >> 18) & 63] + ALPHABET[(n >> 12) & 63] + ALPHABET[(n >> 6) & 63] + ALPHABET[n & 63];
  }
  const rest = bytes.length - i;
  if (rest === 1) {
    const n = bytes[i] << 16;
    out += ALPHABET[(n >> 18) & 63] + ALPHABET[(n >> 12) & 63] + '==';
  } else if (rest === 2) {
    const n = (bytes[i] << 16) | (bytes[i + 1] << 8);
    out += ALPHABET[(n >> 18) & 63] + ALPHABET[(n >> 12) & 63] + ALPHABET[(n >> 6) & 63] + '=';
  }
  return out;
}

// A workspace being wiped (active or not): its font files. The origin is read
// from its slot before the slot's items are deleted.
registerStoredWipeHook('brand_fonts', async (workspaceId) => {
  const origin = await getSecureItemIn(workspaceId, SECURE_KEYS.serverOrigin);
  if (origin !== null) deleteBrandFonts(origin);
});
