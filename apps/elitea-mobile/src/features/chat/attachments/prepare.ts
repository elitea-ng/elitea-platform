import type { AttachmentPolicy } from '@/api/discovery';

/**
 * Attachment checks before anything is uploaded, pure. The server enforces
 * every limit again; these spare the user an upload it would refuse.
 *
 * Limits come from the discovery `attachments` block (client contract 1.1),
 * else the server's documented defaults. Mobile caps one file at 50 MB on
 * top of that: a phone reads it in chunks, but a larger one is not worth
 * the memory, battery or data on a demo.
 */

export const MIB = 1024 * 1024;
/** Mobile's own per-file ceiling. */
export const MOBILE_MAX_FILE_BYTES = 50 * MIB;

/** The server's defaults (v4.2.0 discovery without overrides). */
export const DEFAULT_ATTACHMENT_POLICY: AttachmentPolicy = {
  max_files: 10,
  max_total_bytes: 150 * MIB,
  max_file_bytes: 150 * MIB,
  max_image_bytes: 3 * MIB,
  chunk_bytes: 5 * MIB,
  accepted_extensions: [
    '.csv', '.doc', '.docx', '.gif', '.htm', '.html', '.jpeg', '.jpg', '.json', '.md', '.pdf',
    '.png', '.pptx', '.svg', '.txt', '.webp', '.xls', '.xlsx', '.xml', '.yaml', '.yml',
  ],
  max_extract_bytes: 25 * MIB,
  inline_image_max_bytes: 48 * 1024,
  inline_image_formats: ['.gif', '.jpeg', '.jpg', '.png', '.webp'],
  // An older server does not downscale: mobile fits the first image itself.
  inline_image_downscale: false,
};

/** Photo formats a phone produces that are converted to JPEG before upload. */
export const CONVERTED_EXTENSIONS = ['.heic', '.heif'];
const IMAGE_EXTENSIONS = new Set(['.png', '.jpg', '.jpeg', '.gif', '.webp', '.heic', '.heif']);

export interface AttachmentLimits {
  maxFiles: number;
  maxTotalBytes: number;
  /** One document, after the mobile cap. */
  maxFileBytes: number;
  /** One image as uploaded (after conversion and resizing). */
  maxImageBytes: number;
  chunkBytes: number;
  acceptedExtensions: string[];
  maxExtractBytes: number;
  inlineImageMaxBytes: number;
  /** The server shrinks images for the model itself (1.1); else mobile fits the first one. */
  serverDownscales: boolean;
}

export function attachmentLimits(policy: AttachmentPolicy | null | undefined): AttachmentLimits {
  const p = policy ?? DEFAULT_ATTACHMENT_POLICY;
  const positive = (v: number, fallback: number) => (Number.isSafeInteger(v) && v > 0 ? v : fallback);
  const d = DEFAULT_ATTACHMENT_POLICY;
  return {
    maxFiles: positive(p.max_files, d.max_files),
    maxTotalBytes: positive(p.max_total_bytes, d.max_total_bytes),
    maxFileBytes: Math.min(positive(p.max_file_bytes, d.max_file_bytes), MOBILE_MAX_FILE_BYTES),
    maxImageBytes: Math.min(positive(p.max_image_bytes, d.max_image_bytes), MOBILE_MAX_FILE_BYTES),
    chunkBytes: positive(p.chunk_bytes, d.chunk_bytes),
    acceptedExtensions: [...new Set([...(p.accepted_extensions.length > 0 ? p.accepted_extensions : d.accepted_extensions)].map((e) => e.toLowerCase()))],
    maxExtractBytes: positive(p.max_extract_bytes, d.max_extract_bytes),
    inlineImageMaxBytes: positive(p.inline_image_max_bytes, d.inline_image_max_bytes),
    serverDownscales: policy ? p.inline_image_downscale === true : false,
  };
}

/** `.ext` in lower case, or '' when the name has none. */
export function extensionOf(name: string): string {
  const base = name.slice(name.lastIndexOf('/') + 1);
  const dot = base.lastIndexOf('.');
  return dot > 0 && dot < base.length - 1 ? base.slice(dot).toLowerCase() : '';
}

export type FileKind = 'image' | 'document';

export function kindOf(extension: string): FileKind {
  return IMAGE_EXTENSIONS.has(extension) ? 'image' : 'document';
}

/** A file the user picked (photo library, camera or files). */
export interface PickedFile {
  uri: string;
  name: string;
  /** Bytes, when the picker knew. */
  size: number | null;
  mimeType: string | null;
}

export interface CheckedFile extends PickedFile {
  extension: string;
  kind: FileKind;
  /** HEIC/HEIF: converted to JPEG before upload. */
  convert: boolean;
}

export type RefusalReason = 'type' | 'size' | 'count' | 'total';

export interface Refusal {
  name: string;
  reason: RefusalReason;
  message: string;
}

export interface CheckResult {
  accepted: CheckedFile[];
  refused: Refusal[];
  /** Kept, but worth saying ("the model may not read this"). */
  warnings: string[];
}

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < MIB) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / MIB).toFixed(bytes < 10 * MIB ? 1 : 0)} MB`;
}

/**
 * Checks picked files against the limits, in this order: type, per-file
 * size, count, total. `already` is what the composer holds already.
 */
export function checkFiles(
  picked: readonly PickedFile[],
  limits: AttachmentLimits,
  already: { count: number; totalBytes: number } = { count: 0, totalBytes: 0 },
): CheckResult {
  const accepted: CheckedFile[] = [];
  const refused: Refusal[] = [];
  const warnings: string[] = [];
  let count = already.count;
  let total = already.totalBytes;
  for (const file of picked) {
    const extension = extensionOf(file.name);
    const convert = CONVERTED_EXTENSIONS.includes(extension);
    // 1. type
    if (!convert && !limits.acceptedExtensions.includes(extension)) {
      refused.push({ name: file.name, reason: 'type', message: `${file.name}: this kind of file cannot be attached.` });
      continue;
    }
    const kind = kindOf(extension);
    // 2. per-file size. An image is re-encoded before upload, so its raw
    //    size is held only to the mobile ceiling here; the encoded result
    //    is checked against the image limit afterwards.
    const cap = kind === 'image' ? MOBILE_MAX_FILE_BYTES : limits.maxFileBytes;
    if (file.size !== null && file.size > cap) {
      refused.push({ name: file.name, reason: 'size', message: `${file.name} is larger than ${formatBytes(cap)}.` });
      continue;
    }
    // 3. count
    if (count + 1 > limits.maxFiles) {
      refused.push({ name: file.name, reason: 'count', message: `A message can carry at most ${limits.maxFiles} files.` });
      continue;
    }
    // 4. total
    const size = file.size ?? 0;
    if (total + Math.min(size, kind === 'image' ? limits.maxImageBytes : size) > limits.maxTotalBytes) {
      refused.push({ name: file.name, reason: 'total', message: `These files are larger than ${formatBytes(limits.maxTotalBytes)} together.` });
      continue;
    }
    count += 1;
    total += kind === 'image' ? Math.min(size, limits.maxImageBytes) : size;
    if (kind === 'document' && file.size !== null && file.size > limits.maxExtractBytes) {
      warnings.push(`${file.name} is large: the model may not read all of it.`);
    }
    accepted.push({ ...file, extension, kind, convert });
  }
  return { accepted, refused, warnings };
}

/**
 * The name a file is uploaded under: a safe base (letters, digits, `.`, `_`,
 * `-`; at most 80 characters), a random 4-hex suffix so two files with one
 * name never overwrite each other (the server keys by name), and the
 * lower-case extension (`.jpg` after a conversion).
 */
export function uploadName(name: string, random: () => number = Math.random, extension: string = extensionOf(name)): string {
  const raw = name.slice(name.lastIndexOf('/') + 1);
  const ext = extensionOf(raw);
  const stem = ext ? raw.slice(0, -ext.length) : raw;
  const safe =
    stem
      .normalize('NFKD')
      .replace(/[̀-ͯ]/g, '')
      .replace(/[^A-Za-z0-9._-]+/g, '_')
      .replace(/_+/g, '_')
      .replace(/^[._-]+|[._-]+$/g, '')
      .slice(0, 80) || 'file';
  const suffix = Math.floor(random() * 0x10000)
    .toString(16)
    .padStart(4, '0');
  return `${safe}-${suffix}${extension.toLowerCase()}`;
}

export type Sniffed = 'png' | 'jpeg' | 'gif' | 'webp' | 'heic' | 'pdf' | 'zip';

/** What the first bytes of a file say it is (enough of: 16 bytes). */
export function sniff(bytes: Uint8Array): Sniffed | null {
  const at = (i: number, ...values: number[]) => values.every((v, k) => bytes[i + k] === v);
  const ascii = (i: number, text: string) => at(i, ...[...text].map((c) => c.charCodeAt(0)));
  if (at(0, 0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a)) return 'png';
  if (at(0, 0xff, 0xd8, 0xff)) return 'jpeg';
  if (ascii(0, 'GIF87a') || ascii(0, 'GIF89a')) return 'gif';
  if (ascii(0, 'RIFF') && ascii(8, 'WEBP')) return 'webp';
  if (ascii(4, 'ftyp') && ['heic', 'heix', 'hevc', 'heim', 'heis', 'mif1', 'msf1'].some((brand) => ascii(8, brand))) return 'heic';
  if (ascii(0, '%PDF-')) return 'pdf';
  if (at(0, 0x50, 0x4b, 0x03, 0x04)) return 'zip';
  return null;
}

const IMAGE_SNIFF: Record<string, Sniffed[]> = {
  '.png': ['png'],
  '.jpg': ['jpeg'],
  '.jpeg': ['jpeg'],
  '.gif': ['gif'],
  '.webp': ['webp'],
  '.heic': ['heic'],
  '.heif': ['heic'],
};

/** True when the name claims an image and the bytes are not that image. */
export function imageMismatch(extension: string, sniffed: Sniffed | null): boolean {
  const expected = IMAGE_SNIFF[extension];
  return expected !== undefined && (sniffed === null || !expected.includes(sniffed));
}
