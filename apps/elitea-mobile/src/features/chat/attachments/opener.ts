import { randomUUID } from 'expo-crypto';
import { Directory, File, Paths } from 'expo-file-system';

import { extensionOf, sniff } from '@/features/chat/attachments/prepare';

/**
 * Where an opened attachment lives while it is looked at: one random folder
 * per open under the caches directory, deleted when the viewer closes, before
 * the next open, when the thread goes, at app start, and with the session's
 * caches (logout, revoke, server switch). Downloads are never kept beyond
 * that, whatever the policy: the device stores no attachment at rest.
 */
export const DOWNLOAD_TEMP_DIR = 'att-tmp';

export interface TempFile {
  uri: string;
  delete: () => void;
}

const MAX_NAME = 120;

/**
 * Types the system viewer shows without running anything. Everything else
 * (HTML, SVG, XML, scripts, unknown) is handed over as `.txt`: the server
 * serves those as octet-stream so no client renders them, but Quick Look and
 * an ACTION_VIEW handler pick the renderer from the file's extension.
 */
const INERT_EXTENSIONS: ReadonlySet<string> = new Set([
  '.pdf', '.txt', '.csv', '.md', '.log', '.json', '.rtf',
  '.doc', '.docx', '.xls', '.xlsx', '.ppt', '.pptx', '.odt', '.ods', '.odp',
  '.png', '.jpg', '.jpeg', '.gif', '.webp', '.heic', '.heif',
]);

/** The temporary file's name: one safe path segment, with an inert extension. */
export function tempFileName(name: string): string {
  const safe = name.replace(/[/\\\u0000-\u001f]/g, '_') || 'file';
  const ext = extensionOf(safe);
  if (INERT_EXTENSIONS.has(ext)) return safe.length <= MAX_NAME ? safe : safe.slice(0, MAX_NAME - ext.length) + ext;
  return `${safe.slice(0, MAX_NAME - 4)}.txt`;
}

/** Writes the bytes to a fresh temporary file (named by `tempFileName`). */
export function writeTempFile(bytes: Uint8Array, name: string): TempFile {
  const folder = new Directory(Paths.cache, DOWNLOAD_TEMP_DIR, randomUUID());
  folder.create({ intermediates: true });
  const file = new File(folder, tempFileName(name));
  file.write(bytes);
  return {
    uri: file.uri,
    delete: () => {
      try {
        if (folder.exists) folder.delete();
      } catch {
        // swept later
      }
    },
  };
}

/** Shown in the app's own viewer only when the BYTES are a raster image; anything else goes to the system preview. */
export function viewerFor(bytes: Uint8Array): 'image' | 'preview' {
  const kind = sniff(bytes.subarray(0, 16));
  return kind === 'png' || kind === 'jpeg' || kind === 'gif' || kind === 'webp' ? 'image' : 'preview';
}

/** Opens Quick Look (iOS) or an ACTION_VIEW intent (Android) for a local file. */
export async function previewFile(uri: string): Promise<void> {
  await new File(uri).preview();
}

/** Deletes every opened attachment left behind. */
export function sweepDownloadTemp(): void {
  try {
    const dir = new Directory(Paths.cache, DOWNLOAD_TEMP_DIR);
    if (dir.exists) dir.delete();
  } catch {
    // best effort
  }
}
