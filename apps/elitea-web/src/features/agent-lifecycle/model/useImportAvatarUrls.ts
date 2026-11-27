import { useEffect, useState } from 'react';

import type { ImportSummaryAgent } from '../lib/importSummary';
import { pendingStillOf } from '../lib/importSummary';
import type { PendingAvatar } from '../lib/importTypes';

const MEDIA_TYPES: Readonly<Record<string, string>> = {
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  webp: 'image/webp',
  svg: 'image/svg+xml',
};

function mediaType(path: string): string {
  return MEDIA_TYPES[path.split('.').pop()?.toLowerCase() ?? ''] ?? 'application/octet-stream';
}

/**
 * The preview's face of each agent whose zip carries avatar media (issue
 * 3142): its still as a `blob:` URL, by version key. Nothing is uploaded
 * before the person confirms, so the preview shows the bytes it read; the
 * URLs are revoked when the preview closes. A browser without object URLs
 * (a test DOM) gets none, and the rows fall back to the monogram.
 */
export function useImportAvatarUrls(avatars: readonly PendingAvatar[] | undefined, agents: readonly ImportSummaryAgent[] | undefined): ReadonlyMap<string, string> {
  const [urls, setUrls] = useState<ReadonlyMap<string, string>>(() => new Map());
  useEffect(() => {
    if (avatars === undefined || agents === undefined || typeof URL.createObjectURL !== 'function') {
      setUrls(new Map());
      return undefined;
    }
    const made = new Map<string, string>();
    for (const agent of agents) {
      const still = pendingStillOf(avatars, agent.versionKey);
      if (still === undefined || agent.versionKey === undefined) continue;
      made.set(agent.versionKey, URL.createObjectURL(new Blob([still.bytes as BlobPart], { type: mediaType(still.path) })));
    }
    setUrls(made);
    return () => {
      for (const url of made.values()) URL.revokeObjectURL(url);
    };
  }, [avatars, agents]);
  return urls;
}
