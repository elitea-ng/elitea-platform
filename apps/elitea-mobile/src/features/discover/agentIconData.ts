import type { BaseFetch } from '@/api/middleware';
import { noRedirectFetch } from '@/api/noRedirectFetch';
import { RASTER_TYPES, fetchVettedMedia, mediaTypeOf } from '@/features/media/vettedMedia';
import { MAX_RASTER_BYTES, base64Of } from '@/theme/brandAssets';

/**
 * A public agent's icon as a `data:` URI, or null (SEC-3).
 *
 * Whoever publishes an agent chooses its icon path. `publicAgentIconUrl`
 * keeps it on the workspace origin, but `<Image source={{ uri }}>` hands the
 * URL to the platform loader, which follows a redirect to any host: a
 * same-origin path that answers 3xx (an auth route, an open redirect) would
 * make every device that scrolls Discover request a host the publisher
 * picked, and decode its bytes. So the icon is downloaded the way brand
 * rasters are (`fetchVettedMedia`): through the origin-guarded anonymous
 * fetch with redirects refused, no credential, a 200 with a PNG, JPEG or
 * WebP Content-Type and at most MAX_RASTER_BYTES; the bytes must also BE a
 * PNG, JPEG or WebP, and only they reach `<Image>`.
 */
export async function fetchPublicAgentIcon(uri: string, fetchImpl: BaseFetch = noRedirectFetch): Promise<string | null> {
  const media = await fetchVettedMedia(uri, { maxBytes: MAX_RASTER_BYTES, types: RASTER_TYPES }, fetchImpl);
  return media === null ? null : `data:${media.type};base64,${base64Of(media.bytes)}`;
}

/** The raster type the bytes start with (PNG, JPEG or WebP signature), or null. */
export function rasterTypeOf(bytes: Uint8Array): 'image/png' | 'image/jpeg' | 'image/webp' | null {
  const type = mediaTypeOf(bytes);
  return type === 'image/png' || type === 'image/jpeg' || type === 'image/webp' ? type : null;
}
