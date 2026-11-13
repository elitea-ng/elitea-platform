import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useCallback, useRef } from 'react';

import { publicAgentIconUrl } from '@/api/queries/discover';
import { queryKeys } from '@/api/queries/keys';
import type { BaseFetch } from '@/api/middleware';
import { noRedirectFetch } from '@/api/noRedirectFetch';
import { useSession } from '@/state/session';
import { fetchBrandAsset } from '@/theme/brandAssets';

/**
 * Publisher-chosen media from the workspace origin, downloaded the SEC-3 way (issue 1883: one
 * helper for Discover's agent icons, the chat's agent icon and the avatar pack media).
 *
 * `<Image source={{ uri }}>` and a video player hand a URL to the platform loader, which follows a
 * redirect to any host: a same-origin path that answers 3xx would make every device that shows the
 * agent request a host the publisher picked. So the file is fetched through the origin-guarded
 * anonymous fetch with redirects refused and no credential, it must answer 200 with an allowed
 * Content-Type and at most `maxBytes`, and its bytes must start with the signature of an allowed
 * type. Only those bytes reach a view.
 */

export type VettedMediaType = 'image/png' | 'image/jpeg' | 'image/webp' | 'video/mp4' | 'video/webm';

export const RASTER_TYPES: readonly VettedMediaType[] = ['image/png', 'image/jpeg', 'image/webp'];

/** The type the bytes start with (a PNG, JPEG, WebP, MP4 or WebM signature), or null. */
export function mediaTypeOf(bytes: Uint8Array): VettedMediaType | null {
  const at = (i: number, ...expected: number[]) => expected.every((b, k) => bytes[i + k] === b);
  if (at(0, 0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a)) return 'image/png';
  if (at(0, 0xff, 0xd8, 0xff)) return 'image/jpeg';
  if (at(0, 0x52, 0x49, 0x46, 0x46) && at(8, 0x57, 0x45, 0x42, 0x50)) return 'image/webp';
  // ISO BMFF: the first box is `ftyp` (bytes 4-7).
  if (at(4, 0x66, 0x74, 0x79, 0x70)) return 'video/mp4';
  // Matroska/WebM: the EBML magic.
  if (at(0, 0x1a, 0x45, 0xdf, 0xa3)) return 'video/webm';
  return null;
}

/** What one download accepts. */
export interface VettedMediaRules {
  /** The byte limit: a declared Content-Length over it is refused before the body is read. */
  readonly maxBytes: number;
  /** The types allowed, both as the Content-Type and as the bytes' signature. */
  readonly types: readonly VettedMediaType[];
}

/** A checked download: its bytes and the type its signature names. */
export interface VettedMedia {
  readonly bytes: Uint8Array;
  readonly type: VettedMediaType;
}

/** The checked bytes of `uri`, or null (another origin, a redirect, a refused type, too large, a failure). */
export async function fetchVettedMedia(uri: string, rules: VettedMediaRules, fetchImpl: BaseFetch = noRedirectFetch): Promise<VettedMedia | null> {
  const allowed = new Set<string>(rules.types);
  const bytes = await fetchBrandAsset(uri, { test: (contentType) => allowed.has(contentType) }, rules.maxBytes, fetchImpl);
  if (bytes === null) return null;
  const type = mediaTypeOf(bytes);
  return type !== null && allowed.has(type) ? { bytes, type } : null;
}

/**
 * One vetted media file as `fetcher` turns it into something a view draws, or null while it loads,
 * when it is refused, and for a path that is not on the workspace origin. The answer is cached under
 * the project's agent queries (`segment` keeps the kinds apart), so the session wipe and a project
 * switch drop it. `keep`: a content-addressed file never changes, so it is kept for good (no stale
 * time, no garbage collection) and never downloaded again in the session.
 */
export function useVettedMedia<T>(
  projectId: number | null,
  segment: string,
  path: string | null,
  fetcher: (uri: string) => Promise<T | null>,
  keep = false,
): T | null {
  const origin = useSession((s) => s.serverOrigin);
  const uri = publicAgentIconUrl(path, origin);
  const media = useQuery({
    queryKey: [...queryKeys.agents(projectId ?? 0), segment, uri ?? ''],
    enabled: uri !== null && projectId !== null,
    staleTime: Infinity,
    ...(keep ? { gcTime: Infinity } : {}),
    retry: false,
    queryFn: async () => (await fetcher(uri as string)) ?? null,
  });
  return uri !== null ? (media.data ?? null) : null;
}

/**
 * A function that drops the cached answer for `path` so it is fetched again, for a view whose file
 * failed to load: a `keep` answer is a `file://` URI under the OS caches folder, which the system
 * may evict while the query lives on. Once per path per mount, so a file that is bad on every
 * download cannot loop.
 */
export function useVettedMediaReload(projectId: number | null, segment: string): (path: string | null) => void {
  const queries = useQueryClient();
  const origin = useSession((s) => s.serverOrigin);
  const tried = useRef(new Set<string>());
  return useCallback(
    (path: string | null) => {
      const uri = publicAgentIconUrl(path, origin);
      if (uri === null || projectId === null || tried.current.has(uri)) return;
      tried.current.add(uri);
      void queries.invalidateQueries({ queryKey: [...queryKeys.agents(projectId), segment, uri] });
    },
    [queries, origin, projectId, segment],
  );
}
