import { File, Paths } from 'expo-file-system';
import { Platform } from 'react-native';

import { log } from '@/lib/log';
import { deleteAllSecureItems } from '@/security/secureStorage';

/**
 * Fresh-install purge.
 *
 * The iOS Keychain survives an uninstall: a reinstalled app would find the
 * previous install's token and workspace and sign straight back in. The app's
 * documents directory does NOT survive an uninstall, so a marker file there
 * tells the two apart. No marker means this is the first launch of this
 * install: every registered secure-storage key is deleted, then the marker is
 * written.
 *
 * Android deletes Keystore entries with the app (and `allowBackup` is false),
 * so the check is a harmless no-op there. The web build keeps secure items in
 * memory only and has nothing to purge.
 */

export const INSTALL_MARKER_NAME = '.agent-zefir-install';

export interface InstallMarker {
  exists(): boolean;
  write(contents: string): void;
}

/** The marker file in the documents directory. */
export function defaultInstallMarker(): InstallMarker {
  const file = () => new File(Paths.document, INSTALL_MARKER_NAME);
  return {
    exists: () => file().exists,
    write: (contents) => {
      const f = file();
      if (!f.exists) f.create();
      f.write(contents);
    },
  };
}

export interface FreshInstallResult {
  /** True when no marker was found, i.e. the first launch of this install. */
  fresh: boolean;
  /** True when a fresh install's purge failed for some key (the marker is then not written). */
  purgeIncomplete: boolean;
}

export interface FreshInstallOptions {
  marker?: InstallMarker;
  purge?: typeof deleteAllSecureItems;
  now?: () => number;
  platform?: string;
}

/**
 * Purges secure storage on the first launch of an install. Never throws.
 *
 * The marker is written only after a complete purge, so an interrupted or
 * failed purge is retried on the next launch; the caller must treat a fresh
 * install as signed out either way (`fresh: true`).
 */
export async function checkFreshInstall(options: FreshInstallOptions = {}): Promise<FreshInstallResult> {
  if ((options.platform ?? Platform.OS) === 'web') return { fresh: false, purgeIncomplete: false };
  const marker = options.marker ?? defaultInstallMarker();
  const purge = options.purge ?? deleteAllSecureItems;

  let present: boolean;
  try {
    present = marker.exists();
  } catch (error) {
    // Unreadable documents directory: do not purge on a guess (that would look
    // like a logout); the next launch checks again.
    log.warn('install marker could not be read', { error });
    return { fresh: false, purgeIncomplete: false };
  }
  if (present) return { fresh: false, purgeIncomplete: false };

  let failures: Awaited<ReturnType<typeof deleteAllSecureItems>>;
  try {
    failures = await purge();
  } catch (error) {
    log.warn('fresh-install purge failed', { error });
    return { fresh: true, purgeIncomplete: true };
  }
  if (failures.length > 0) {
    log.warn('fresh-install purge could not delete some keys', { keys: failures.map((f) => f.key) });
    return { fresh: true, purgeIncomplete: true };
  }
  try {
    marker.write(JSON.stringify({ installedAt: (options.now ?? Date.now)() }));
  } catch (error) {
    log.warn('install marker could not be written', { error });
  }
  return { fresh: true, purgeIncomplete: false };
}
