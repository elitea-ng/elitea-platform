import { File } from 'expo-file-system';
import { ImageManipulator, SaveFormat } from 'expo-image-manipulator';

import { formatBytes, imageMismatch, sniff, uploadName, type AttachmentLimits, type CheckedFile } from '@/features/chat/attachments/prepare';

/**
 * Photos before upload: HEIC/HEIF become JPEG, JPEG is always re-encoded
 * (which drops EXIF, GPS included), a PNG or WebP within the limit is
 * re-written losslessly as PNG (its metadata chunks go too), and any image
 * over 2048 px on its long
 * edge or over the image limit is fitted. Quality steps down, then the size,
 * until it fits (never below 256 px). GIFs are kept as they are.
 *
 * Every file's first bytes are checked against its name: an image that is
 * not one is refused.
 */

export const LONG_EDGE = 2048;
const MIN_EDGE = 256;
const QUALITIES = [0.8, 0.65, 0.5, 0.4];

export interface ReadyFile {
  /** A local file to upload (a re-encoded copy in the cache, or the picked one). */
  uri: string;
  /** The upload name (sanitised, suffixed, `.jpg` after a conversion). */
  uploadName: string;
  /** What the user picked it as. */
  displayName: string;
  size: number;
  mimeType: string;
  kind: CheckedFile['kind'];
  /** The re-encoded copy is ours to delete after the upload. */
  temporary: boolean;
}

export class PrepareError extends Error {
  constructor(readonly userMessage: string) {
    super(userMessage);
    this.name = 'PrepareError';
  }
}

const MIME_BY_EXTENSION: Record<string, string> = {
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.gif': 'image/gif',
  '.webp': 'image/webp',
  '.pdf': 'application/pdf',
  '.txt': 'text/plain',
  '.md': 'text/markdown',
  '.csv': 'text/csv',
  '.json': 'application/json',
  '.xml': 'application/xml',
  '.yaml': 'application/yaml',
  '.yml': 'application/yaml',
  '.html': 'text/html',
  '.htm': 'text/html',
  '.svg': 'image/svg+xml',
  '.doc': 'application/msword',
  '.docx': 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  '.xls': 'application/vnd.ms-excel',
  '.xlsx': 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  '.pptx': 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
};

export function mimeOf(extension: string): string {
  return MIME_BY_EXTENSION[extension] ?? 'application/octet-stream';
}

export interface PipelineDeps {
  /** File access (tests inject a fake). */
  openFile?: (uri: string) => { size: number; readHead: () => Promise<Uint8Array>; delete: () => void };
  /**
   * Re-encodes `uri` fitted to `edge` px at `quality` (JPEG unless `format`
   * says PNG); resolves to the new file's uri and size. Either way only the
   * pixels are written: no EXIF, XMP or text chunks.
   */
  encode?: (
    uri: string,
    edge: number | null,
    quality: number,
    format?: 'jpeg' | 'png',
  ) => Promise<{ uri: string; size: number; width: number; height: number }>;
  random?: () => number;
}

function defaultOpenFile(uri: string) {
  const file = new File(uri);
  return {
    size: file.size,
    readHead: async () => {
      const handle = file.open();
      try {
        return await handle.readBytes(16);
      } finally {
        handle.close();
      }
    },
    delete: () => {
      try {
        if (file.exists) file.delete();
      } catch {
        // best effort: the caches are wiped with the session anyway
      }
    },
  };
}

async function defaultEncode(uri: string, edge: number | null, quality: number, format: 'jpeg' | 'png' = 'jpeg') {
  const context = ImageManipulator.manipulate(uri);
  const original = await context.renderAsync();
  const long = Math.max(original.width, original.height);
  let ref = original;
  if (edge !== null && long > edge) {
    const scale = edge / long;
    context.resize({ width: Math.round(original.width * scale), height: Math.round(original.height * scale) });
    ref = await context.renderAsync();
  }
  const saved = await ref.saveAsync(format === 'png' ? { format: SaveFormat.PNG } : { format: SaveFormat.JPEG, compress: quality });
  return { uri: saved.uri, size: new File(saved.uri).size, width: saved.width, height: saved.height };
}

/**
 * Makes one checked file ready to upload. `fitInline` (the first image of a
 * turn, when the server does not downscale) fits it to the inline size the
 * model receives instead of the image limit.
 */
export async function readyFile(
  file: CheckedFile,
  limits: AttachmentLimits,
  options: { fitInline?: boolean } = {},
  deps: PipelineDeps = {},
): Promise<ReadyFile> {
  const open = deps.openFile ?? defaultOpenFile;
  const encode = deps.encode ?? defaultEncode;
  const source = open(file.uri);
  const head = await source.readHead().catch(() => new Uint8Array());
  if (imageMismatch(file.extension, sniff(head))) {
    throw new PrepareError(`${file.name} is not the image its name says it is.`);
  }

  if (file.kind !== 'image' || file.extension === '.gif') {
    const limit = file.kind === 'image' ? limits.maxImageBytes : limits.maxFileBytes;
    if (source.size > limit) throw new PrepareError(`${file.name} is larger than ${formatBytes(limit)}.`);
    return {
      uri: file.uri,
      uploadName: uploadName(file.name, deps.random),
      displayName: file.name,
      size: source.size,
      mimeType: mimeOf(file.extension),
      kind: file.kind,
      temporary: false,
    };
  }

  const target = options.fitInline && !limits.serverDownscales ? Math.min(limits.maxImageBytes, limits.inlineImageMaxBytes) : limits.maxImageBytes;
  const reencode = file.convert || file.extension === '.jpg' || file.extension === '.jpeg' || source.size > target || options.fitInline === true;
  if (!reencode) {
    // A PNG or WebP within the limit keeps its pixels, but never the file as
    // picked: PNG eXIf/iTXt and WebP EXIF/XMP chunks can carry GPS, and
    // `exif: false` on the picker only leaves EXIF out of the asset object.
    // A lossless PNG re-write keeps pixels and transparency, and drops them.
    const clean = await encode(file.uri, null, 1, 'png');
    if (clean.size <= target) {
      return {
        uri: clean.uri,
        uploadName: uploadName(file.name, deps.random, '.png'),
        displayName: file.name,
        size: clean.size,
        mimeType: 'image/png',
        kind: 'image',
        temporary: true,
      };
    }
    // Larger than the limit once re-written: the JPEG steps below.
    open(clean.uri).delete();
  }

  let edge = LONG_EDGE;
  let made: { uri: string; size: number } | null = null;
  for (;;) {
    for (const quality of QUALITIES) {
      if (made) open(made.uri).delete();
      made = await encode(file.uri, edge, quality);
      if (made.size <= target) {
        return {
          uri: made.uri,
          uploadName: uploadName(file.name, deps.random, '.jpg'),
          displayName: file.name,
          size: made.size,
          mimeType: 'image/jpeg',
          kind: 'image',
          temporary: true,
        };
      }
    }
    if (edge <= MIN_EDGE) break;
    edge = Math.max(MIN_EDGE, Math.round(edge * 0.6));
  }
  if (made) open(made.uri).delete();
  // The inline fit is a nicety: an image that cannot reach it is still sent
  // within the ordinary image limit.
  if (options.fitInline) return readyFile(file, limits, { fitInline: false }, deps);
  throw new PrepareError(`${file.name} could not be made smaller than ${formatBytes(target)}.`);
}
