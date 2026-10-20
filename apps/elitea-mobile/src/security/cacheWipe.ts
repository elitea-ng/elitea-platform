import { Directory, Paths } from 'expo-file-system';
import { Platform } from 'react-native';

/**
 * Deletes everything in the app's caches directory (`Library/Caches` on iOS,
 * `cacheDir` on Android) as part of the session wipe.
 *
 * The directory holds the platform URL cache (`Library/Caches/<bundle id>/Cache.db*`
 * on iOS). The app turns that cache off at launch (plugins/withNoHttpCache.js),
 * but a file written by an earlier build, or by any library that keeps its own
 * cache there, must not survive "Log out & wipe" or a revoke (DEF-R2). Nothing
 * the app needs to keep lives there: the offline store is in the documents
 * directory and is wiped by its own hook.
 *
 * The directory itself belongs to the OS and stays; its children are deleted.
 * Every child is attempted; the first failure is rethrown after the rest have
 * been tried, so `endSession` records the step as failed.
 */

export interface CacheEntry {
  delete(): void;
}

export interface CacheDirectory {
  readonly exists: boolean;
  list(): CacheEntry[];
}

export function defaultCacheDirectory(): CacheDirectory {
  return new Directory(Paths.cache);
}

export function wipeCachesDirectory(
  directory: CacheDirectory = defaultCacheDirectory(),
  platform: string = Platform.OS,
): void {
  if (platform === 'web') return;
  if (!directory.exists) return;
  let firstError: unknown = null;
  for (const entry of directory.list()) {
    try {
      entry.delete();
    } catch (error) {
      firstError ??= error;
    }
  }
  if (firstError !== null) throw firstError;
}
