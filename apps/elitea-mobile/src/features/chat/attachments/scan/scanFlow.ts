import type { AttachmentLimits, PickedFile } from '@/features/chat/attachments/prepare';
import { ScanError, scanFormats, scanRoom, scanToFiles, type ScanFormat, type ScanPipelineDeps } from '@/features/chat/attachments/scan/scanPipeline';
import type { DocumentScannerNative } from '@/features/chat/attachments/scan/scanner';

/**
 * "Scan document" from the attach menu, start to tray: scan (or the development sample
 * pages), ask PDF or text when the workspace takes both, build the file. Everything of a
 * scan lives in one directory under the caches' DocumentScan directory, deleted here unless
 * a file from it goes to the tray (which deletes that file after the upload; the directory
 * itself is swept at the next start and when the workspace is put aside or ended).
 */

export type ScanOutcome =
  | { kind: 'cancelled' }
  | { kind: 'files'; files: PickedFile[]; warnings: string[] }
  | { kind: 'refused'; message: string };

export interface ScanFlowDeps extends ScanPipelineDeps {
  source: 'camera' | 'samples';
  native: DocumentScannerNative;
  limits: AttachmentLimits;
  /** A new, empty directory for this scan. */
  makeDirectory: () => { uri: string; remove: () => void };
  /** Asked only when the workspace takes both; null: the user cancelled. */
  chooseFormat: (formats: ScanFormat[], pageCount: number) => Promise<ScanFormat | null>;
  /** The development sample pages, written into `directory`. */
  samplePages?: (directory: string) => Promise<string[]>;
  /** False once the screen that asked is gone: the scan is then thrown away. */
  isCurrent: () => boolean;
  /**
   * What the message carries already (the tray's `usage()`), read before the camera opens and
   * again when the scan is built: the scan is fitted to the room left, never refused after.
   */
  held?: () => { count: number; totalBytes: number };
  /**
   * The slow part starts: the camera is closed and the format chosen, and the PDF or text is
   * now being made (on-device recognition of every page, seconds for a long scan).
   */
  onPreparing?: () => void;
}

export const NO_SCAN_FORMAT = 'This workspace takes neither PDF nor text files, so a scan cannot be attached here.';

export async function runScan(deps: ScanFlowDeps): Promise<ScanOutcome> {
  const formats = scanFormats(deps.limits);
  if (formats.length === 0) return { kind: 'refused', message: NO_SCAN_FORMAT };
  const held = deps.held ?? (() => ({ count: 0, totalBytes: 0 }));
  const before = scanRoom(deps.limits, held());
  if ('refused' in before) return { kind: 'refused', message: before.refused };
  const directory = deps.makeDirectory();
  let kept = false;
  try {
    const pages =
      deps.source === 'camera' ? await deps.native.scan(directory.uri) : deps.samplePages ? await deps.samplePages(directory.uri) : [];
    if (pages.length === 0 || !deps.isCurrent()) return { kind: 'cancelled' };
    const format = formats.length === 1 ? formats[0] : await deps.chooseFormat(formats, pages.length);
    if (format === null || !deps.isCurrent()) return { kind: 'cancelled' };
    // The tray may have changed while the camera was open.
    const room = scanRoom(deps.limits, held());
    if ('refused' in room) return { kind: 'refused', message: room.refused };
    deps.onPreparing?.();
    const built = await scanToFiles(pages, format, directory.uri, deps.limits, deps, room.bytes);
    if (!deps.isCurrent() || built.files.length === 0) return { kind: 'cancelled' };
    kept = true;
    return { kind: 'files', files: built.files, warnings: built.warnings };
  } catch (error) {
    if (error instanceof ScanError) return { kind: 'refused', message: error.userMessage };
    throw error;
  } finally {
    if (!kept) directory.remove();
  }
}
