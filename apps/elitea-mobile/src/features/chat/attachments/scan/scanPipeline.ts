import type { DocumentScannerNative } from '@/features/chat/attachments/scan/scanner';
import { formatBytes, type AttachmentLimits, type PickedFile } from '@/features/chat/attachments/prepare';

/**
 * A scan's pages → the file(s) the composer attaches, pure apart from the injected native
 * and file functions:
 *
 * - `pdf`: one PDF of every page, with the text recognised on the device as an invisible
 *   text layer (a scanned page without one reaches the model as no text at all). Fitted to
 *   the room the message has left (the per-file limit, or less next to files already in the
 *   tray) by stepping the page size and JPEG quality down.
 * - `text`: the recognised text alone as a `.txt` file, for text-only models and to spend
 *   fewer tokens.
 *
 * The page images are always deleted when this returns or throws: only the PDF or the text
 * file is kept, and that one is the tray's (deleted after the upload, see pickers.ts).
 */

export type ScanFormat = 'pdf' | 'text';

/** Under the app's caches directory (swept at start and when a workspace is put aside). */
export const SCAN_CACHE_DIR = 'DocumentScan';
/** VisionKit's document camera keeps its own scratch files here, also under the caches. */
export const VISIONKIT_CACHE_DIR = 'com.apple.DocumentCamera';

export class ScanError extends Error {
  constructor(readonly userMessage: string) {
    super(userMessage);
    this.name = 'ScanError';
  }
}

/** The formats the workspace takes (its accepted extensions), PDF first. */
export function scanFormats(limits: AttachmentLimits): ScanFormat[] {
  const formats: ScanFormat[] = [];
  if (limits.acceptedExtensions.includes('.pdf')) formats.push('pdf');
  if (limits.acceptedExtensions.includes('.txt')) formats.push('text');
  return formats;
}

/** Page size (long edge, px) and JPEG quality, tried in order until the PDF fits. */
export const PDF_STEPS: readonly { edge: number; quality: number }[] = [
  { edge: 2048, quality: 0.7 },
  { edge: 2048, quality: 0.5 },
  { edge: 1600, quality: 0.5 },
  { edge: 1280, quality: 0.4 },
  { edge: 1024, quality: 0.35 },
];

const pad = (n: number) => String(n).padStart(2, '0');

/** "Scan 2026-10-08 14.05.pdf": the name sent with the file (the tray shows `scanDisplay`). */
export function scanName(date: Date, extension: '.pdf' | '.txt'): string {
  const day = `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
  return `Scan ${day} ${pad(date.getHours())}.${pad(date.getMinutes())}${extension}`;
}

/**
 * The tray chip's short form of the scan's name: "Scan 14.05" over "Oct 8 · 1.2 MB", so the
 * date (what tells two scans apart) is never lost to the chip's truncation of the full name.
 */
export function scanDisplay(date: Date): { title: string; detail: string } {
  return { title: `Scan ${pad(date.getHours())}.${pad(date.getMinutes())}`, detail: date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) };
}

/** The pages' text as one document; each page under a heading when there are several. */
export function scanText(pages: readonly string[]): string {
  const texts = pages.map((t) => t.trim());
  if (texts.length === 1) return `${texts[0]}\n`;
  return `${texts.map((t, i) => `--- Page ${i + 1} of ${texts.length} ---\n\n${t}`).join('\n\n')}\n`;
}

export interface ScanPipelineDeps {
  native: Pick<DocumentScannerNative, 'recognizeLayout' | 'makePdf' | 'recognizeText' | 'writeText'>;
  /** Deletes a file; never throws. */
  remove: (uri: string) => void;
  now?: () => Date;
}

export interface ScanFiles {
  files: PickedFile[];
  /** Kept, but worth saying. */
  warnings: string[];
}

/**
 * Builds the attachment for `pages` in `format`, inside `directory` (a file URI ending in
 * `/`, under the caches' DocumentScan directory), no larger than `room` bytes: the per-file
 * limit, or less when the message already carries files (see `scanRoom`).
 */
export async function scanToFiles(
  pages: readonly string[],
  format: ScanFormat,
  directory: string,
  limits: AttachmentLimits,
  deps: ScanPipelineDeps,
  room: number = limits.maxFileBytes,
): Promise<ScanFiles> {
  const now = (deps.now ?? (() => new Date()))();
  const fit = Math.min(room, limits.maxFileBytes);
  // The per-file limit, or the room left next to the files the message carries already.
  const tooLarge = (what: string) =>
    fit < limits.maxFileBytes
      ? `${what} is larger than the ${formatBytes(fit)} left in this message. Scan fewer pages or remove a file.`
      : `${what} is larger than ${formatBytes(limits.maxFileBytes)}. Scan fewer pages.`;
  try {
    if (pages.length === 0) return { files: [], warnings: [] };
    if (format === 'pdf') {
      const output = `${directory}scan.pdf`;
      // Recognised once: every size step below re-encodes the images only.
      const recognised = await deps.native.recognizeLayout([...pages]);
      const blank = recognised.text.every((t) => t.trim() === '');
      for (const step of PDF_STEPS) {
        const made = await deps.native.makePdf([...pages], output, step.edge, step.quality, recognised.layout);
        if (made.size <= fit) {
          return {
            files: [{ uri: made.uri, name: scanName(now, '.pdf'), size: made.size, mimeType: 'application/pdf', display: scanDisplay(now) }],
            warnings: blank ? ['No text was found on the scanned pages: the model may not be able to read them.'] : [],
          };
        }
        deps.remove(made.uri);
      }
      throw new ScanError(tooLarge('The scan'));
    }
    const texts = await deps.native.recognizeText([...pages]);
    if (texts.every((t) => t.trim() === '')) throw new ScanError('No text was found on the scanned pages.');
    const output = `${directory}scan.txt`;
    const size = await deps.native.writeText(output, scanText(texts));
    if (size > fit) {
      deps.remove(output);
      throw new ScanError(tooLarge('The scanned text'));
    }
    return { files: [{ uri: output, name: scanName(now, '.txt'), size, mimeType: 'text/plain', display: scanDisplay(now) }], warnings: [] };
  } finally {
    for (const page of pages) deps.remove(page);
  }
}

/**
 * The bytes a scan may take next to what the message `held` already: the per-file limit, or
 * the total left when that is less. Null: the message can take no further file (the count or
 * the total is spent), said in the words the tray would refuse it with.
 */
export function scanRoom(limits: AttachmentLimits, held: { count: number; totalBytes: number }): { bytes: number } | { refused: string } {
  if (held.count + 1 > limits.maxFiles) return { refused: `A message can carry at most ${limits.maxFiles} files.` };
  const left = limits.maxTotalBytes - held.totalBytes;
  if (left <= 0) return { refused: `These files are larger than ${formatBytes(limits.maxTotalBytes)} together.` };
  return { bytes: Math.min(limits.maxFileBytes, left) };
}
