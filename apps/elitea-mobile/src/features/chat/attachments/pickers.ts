import * as DocumentPicker from 'expo-document-picker';
import { Directory, File, Paths } from 'expo-file-system';
import * as ImagePicker from 'expo-image-picker';

import type { PickedFile } from '@/features/chat/attachments/prepare';
import { scanInFlight } from '@/features/chat/attachments/scan/scanInFlight';

/** VisionKit's own scratch directory (scan/scanPipeline.ts VISIONKIT_CACHE_DIR). */
const VISIONKIT_SCRATCH = 'com.apple.DocumentCamera';

/**
 * The three ways to pick files. Each asks for its permission only when used
 * and answers [] when the user cancels or refuses.
 */

export class PickerPermissionError extends Error {
  constructor(readonly what: 'camera' | 'photos') {
    super(`${what} permission refused`);
    this.name = 'PickerPermissionError';
  }
}

function fromAssets(assets: readonly ImagePicker.ImagePickerAsset[], fallback: string): PickedFile[] {
  return assets.map((asset, index) => ({
    uri: asset.uri,
    name: asset.fileName?.trim() || `${fallback}-${index + 1}.${asset.mimeType === 'image/png' ? 'png' : 'jpg'}`,
    size: typeof asset.fileSize === 'number' ? asset.fileSize : null,
    mimeType: asset.mimeType ?? null,
  }));
}

export async function pickPhotos(limit: number): Promise<PickedFile[]> {
  const result = await ImagePicker.launchImageLibraryAsync({
    mediaTypes: ['images'],
    allowsMultipleSelection: true,
    selectionLimit: Math.max(1, limit),
    quality: 1,
    exif: false,
  });
  return result.canceled ? [] : fromAssets(result.assets, 'photo');
}

export async function takePhoto(): Promise<PickedFile[]> {
  const permission = await ImagePicker.requestCameraPermissionsAsync();
  if (!permission.granted) throw new PickerPermissionError('camera');
  const result = await ImagePicker.launchCameraAsync({ mediaTypes: ['images'], quality: 1, exif: false });
  return result.canceled ? [] : fromAssets(result.assets, 'camera');
}

export async function pickFiles(): Promise<PickedFile[]> {
  const result = await DocumentPicker.getDocumentAsync({ multiple: true, copyToCacheDirectory: true, type: '*/*' });
  if (result.canceled) return [];
  return result.assets.map((asset) => ({
    uri: asset.uri,
    name: asset.name,
    size: typeof asset.size === 'number' ? asset.size : null,
    mimeType: asset.mimeType ?? null,
  }));
}

/**
 * Where the pickers and the image re-encode leave their copies, under the
 * app's cache: expo-image-picker ("ImagePicker", the library and the camera),
 * expo-document-picker ("DocumentPicker", copyToCacheDirectory),
 * expo-image-manipulator ("ImageManipulator"), the document scan
 * ("DocumentScan": its pages, PDF and recognised text, scan/scanPipeline.ts)
 * and VisionKit's own scratch directory ("com.apple.DocumentCamera", which
 * the document camera creates in the app's caches).
 */
export const PICKER_CACHE_DIRS = ['ImagePicker', 'DocumentPicker', 'ImageManipulator', 'DocumentScan', 'com.apple.DocumentCamera'] as const;

/** True for a file one of the pickers copied into the app's cache (ours to delete). */
export function isPickerCopy(uri: string, cacheUri: string = Paths.cache.uri): boolean {
  const base = cacheUri.endsWith('/') ? cacheUri : `${cacheUri}/`;
  if (!uri.startsWith(base) || uri.includes('/../')) return false;
  const rest = uri.slice(base.length);
  return PICKER_CACHE_DIRS.some((dir) => rest.startsWith(`${dir}/`) && rest.length > dir.length + 1);
}

/** Deletes a picked file's copy when the app owns it; leaves anything else alone. */
export function discardPickerCopy(uri: string): void {
  if (!isPickerCopy(uri)) return;
  try {
    const file = new File(uri);
    if (file.exists) file.delete();
  } catch {
    // swept at the next start, and when the workspace is put aside or ended (tempWipe.ts)
  }
}

/**
 * Deletes the picker copies a crash or a kill left behind (app start), and those of a workspace
 * put aside. VisionKit's scratch directory is left alone while a scan runs (its camera may be
 * capturing into it); that scan deletes it when it ends.
 */
export function sweepPickerCopies(): void {
  for (const name of PICKER_CACHE_DIRS) {
    if (name === VISIONKIT_SCRATCH && scanInFlight()) continue;
    try {
      const dir = new Directory(Paths.cache, name);
      if (dir.exists) dir.delete();
    } catch {
      // best effort
    }
  }
}
