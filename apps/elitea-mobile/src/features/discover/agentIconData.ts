import type { BaseFetch } from '@/api/middleware';
import { noRedirectFetch } from '@/api/noRedirectFetch';
import { MAX_RASTER_BYTES, base64Of, fetchBrandAsset } from '@/theme/brandAssets';

/**
 * A public agent's icon as a `data:` URI, or null (SEC-3).
 *
 * Whoever publishes an agent chooses its icon path. `publicAgentIconUrl`
 * keeps it on the workspace origin, but `<Image source={{ uri }}>` hands the
 * URL to the platform loader, which follows a redirect to any host: a
 * same-origin path that answers 3xx (an auth route, an open redirect) would
 * make every device that scrolls Discover request a host the publisher
 * picked, and decode its bytes. So the icon is downloaded the way brand
 * rasters are: through the origin-guarded anonymous fetch with redirects
 * refused, no credential, a 200 with an image Content-Type and at most
 * MAX_RASTER_BYTES; the bytes must also BE a PNG, JPEG or WebP, and only
 * they reach `<Image>`.
 */
export async function fetchPublicAgentIcon(uri: string, fetchImpl: BaseFetch = noRedirectFetch): Promise<string | null> {
  const bytes = await fetchBrandAsset(uri, /^image\/(png|jpeg|webp)$/, MAX_RASTER_BYTES, fetchImpl);
  if (bytes === null) return null;
  const mime = rasterTypeOf(bytes);
  return mime === null ? null : `data:${mime};base64,${base64Of(bytes)}`;
}

/** The raster type the bytes start with (PNG, JPEG or WebP signature), or null. */
export function rasterTypeOf(bytes: Uint8Array): 'image/png' | 'image/jpeg' | 'image/webp' | null {
  const at = (i: number, ...expected: number[]) => expected.every((b, k) => bytes[i + k] === b);
  if (at(0, 0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a)) return 'image/png';
  if (at(0, 0xff, 0xd8, 0xff)) return 'image/jpeg';
  if (at(0, 0x52, 0x49, 0x46, 0x46) && at(8, 0x57, 0x45, 0x42, 0x50)) return 'image/webp';
  return null;
}
