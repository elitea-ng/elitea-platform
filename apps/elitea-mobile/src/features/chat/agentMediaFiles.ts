import { Directory, File, Paths } from 'expo-file-system';

import { originOf } from '@/api/paths';
import { log } from '@/lib/log';
import { sha256Hex } from '@/lib/sha256';
import { registerStoredWipeHook } from '@/security/endSession';
import { SECURE_KEYS, getSecureItemIn } from '@/security/secureStorage';

/**
 * Checked avatar pack media files on disk (issue 1882).
 *
 * A pack file is up to 2 MiB. Handing it to the view as a `data:` URI meant base64-encoding it on
 * the JS thread mid-stream and keeping the string in the query cache. Instead the checked bytes are
 * written once to `<caches>/agent-media/<digest of origin>/<sha256>.<ext>` and the view gets the
 * file's URI. The name is the server's content address, so a file already on disk is the same bytes
 * and is not downloaded again, on a later turn or after a relaunch.
 *
 * Only bytes that passed the SEC-3 check (`fetchVettedMedia`) are written, and nothing else reads
 * the folder. The folder is per workspace origin and deleted with the workspace, even when it is
 * not the active one, as the brand fonts are: the files show what its agents look like.
 */

export const AGENT_MEDIA_DIR = 'agent-media';

/** The file name a pack url ends in (`<sha256>.<ext>`), or null for anything else. */
export function agentMediaFileName(uri: string): string | null {
  const path = uri.split(/[?#]/, 1)[0];
  return path.match(/\/api\/v2\/agent_media\/[1-9][0-9]{0,17}\/([0-9a-f]{64}\.(?:png|jpg|webp|mp4|webm))$/)?.[1] ?? null;
}

function mediaDirectory(origin: string): Directory {
  return new Directory(Paths.cache, AGENT_MEDIA_DIR, sha256Hex(origin).slice(0, 32));
}

/** Where pack media files are kept; the app's own, a fake in tests. */
export interface AgentMediaFiles {
  /** The URI of the file already on disk for `uri`, or null. */
  readonly cached: (uri: string) => string | null;
  /** Writes checked bytes for `uri`; their file URI, or null when they could not be stored. */
  readonly store: (uri: string, bytes: Uint8Array) => string | null;
}

function fileFor(uri: string): File | null {
  const origin = originOf(uri);
  const name = agentMediaFileName(uri);
  if (origin === null || name === null) return null;
  return new File(mediaDirectory(origin), name);
}

export const agentMediaFiles: AgentMediaFiles = {
  cached(uri) {
    try {
      const file = fileFor(uri);
      return file !== null && file.exists ? file.uri : null;
    } catch {
      return null;
    }
  },
  store(uri, bytes) {
    const origin = originOf(uri);
    if (origin === null) return null;
    try {
      const file = fileFor(uri);
      if (file === null) return null;
      const directory = mediaDirectory(origin);
      if (!directory.exists) directory.create({ intermediates: true });
      if (file.exists) file.delete();
      file.create();
      file.write(bytes);
      return file.uri;
    } catch (error) {
      log.info('agent media not stored', { error });
      return null;
    }
  },
};

/** Deletes the pack media files kept for `origin`. Throws when they could not be deleted. */
export function deleteAgentMediaFiles(origin: string): void {
  const directory = mediaDirectory(origin);
  if (directory.exists) directory.delete();
}

// A workspace being wiped (active or not): its pack media files. The origin is read from its slot
// before the slot's items are deleted.
registerStoredWipeHook('agent_media_files', async (workspaceId) => {
  const origin = await getSecureItemIn(workspaceId, SECURE_KEYS.serverOrigin);
  if (origin !== null) deleteAgentMediaFiles(origin);
});
