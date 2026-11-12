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

// Directories whose temporary files were swept this run.
const swept = new Set<string>();

/**
 * Deletes the `*.part` files a write cut short (the app killed mid-write) left behind. Once per
 * directory per run, before the first write, so it never races this run's own writes.
 */
function sweepPartFiles(directory: Directory): void {
  if (swept.has(directory.uri)) return;
  swept.add(directory.uri);
  try {
    for (const entry of directory.list()) {
      if (entry instanceof File && entry.name.endsWith('.part')) entry.delete();
    }
  } catch (error) {
    log.info('agent media: stale temporary files not swept', { error });
  }
}

/** Forgets which folders were swept (tests). */
export function resetPartFileSweepForTests(): void {
  swept.clear();
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
      sweepPartFiles(directory);
      // Written beside it under a temporary name, then moved into place: `cached` trusts any file
      // with the final name, so a write cut short (the app killed, the disk full) must never
      // leave one there.
      const part = new File(directory, `${file.name}.${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}.part`);
      try {
        part.create();
        part.write(bytes);
        part.moveSync(file, { overwrite: true });
      } catch (error) {
        if (part.exists) part.delete();
        throw error;
      }
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
  swept.delete(directory.uri);
}

// A workspace being wiped (active or not): its pack media files. The origin is read from its slot
// before the slot's items are deleted.
registerStoredWipeHook('agent_media_files', async (workspaceId) => {
  const origin = await getSecureItemIn(workspaceId, SECURE_KEYS.serverOrigin);
  if (origin !== null) deleteAgentMediaFiles(origin);
});
