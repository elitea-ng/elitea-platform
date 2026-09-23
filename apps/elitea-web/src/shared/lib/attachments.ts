/**
 * Attachment size/count limits ported from
 * apps/elitea-ui/src/common/constants.js:1059-1065 (unit S3, spec §9.3).
 */
export const ATTACHMENT_LIMITS = {
  MAX_ATTACHMENTS: 10,
  /** 150MB in bytes. */
  MAX_TOTAL_SIZE: 150 * 1024 * 1024,
  /** 150MB in bytes, only one file. */
  DEFAULT_MAX_FILE_SIZE: 150 * 1024 * 1024,
  MAX_IMAGE_ATTACHMENTS: 10,
  /** 5MB in bytes, per image file (excluding SVG). */
  MAX_IMAGE_FILE_SIZE: 5 * 1024 * 1024,
} as const;

/** SVG gets the default (non-image) size cap, not `MAX_IMAGE_FILE_SIZE` — matches this constant's own doc comment ("excluding SVG"). Not exported — only `validateAttachmentFiles` below reads it today; promote if a second caller needs it directly. */
function isImageFile(file: File): boolean {
  return file.type.startsWith('image/') && file.type !== 'image/svg+xml';
}

/** Not exported — only `validateAttachmentFiles` below reads it today; promote if a second caller needs it directly. */
function formatFileSize(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)}KB`;
  return `${bytes}B`;
}

/**
 * Lower-cases and guarantees the leading dot, so `PDF`, `.PDF` and `pdf` are
 * one key. A local copy of `entities/attachment`'s `normaliseExtension`:
 * `shared/` may not import upward from `entities/`, and the rule is two lines.
 */
function normaliseAttachmentExtension(raw: string): string {
  const trimmed = raw.trim().toLowerCase();
  if (trimmed === '') return '';
  return trimmed.startsWith('.') ? trimmed : `.${trimmed}`;
}

/** The normalised extension of a file name; `''` when it has none. A dotfile (`.env`) counts as having none — its dot is at index 0. */
function attachmentExtension(name: string): string {
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || dot === name.length - 1) return '';
  return normaliseAttachmentExtension(name.slice(dot));
}

/**
 * The type check, as its own function: `validateAttachmentFiles` sits at the
 * §3.5 cyclomatic-complexity-12 ceiling and this branch alone is three of its
 * decisions.
 *
 * `null` means "accepted". `allowed === undefined` means the caller has no
 * list and every file is accepted — see `validateAttachmentFiles`'s own doc
 * comment for why that is NOT the same as an empty list.
 */
function allowedExtensionSet(allowedExtensions: readonly string[] | undefined): ReadonlySet<string> | undefined {
  if (allowedExtensions === undefined) return undefined;
  return new Set(allowedExtensions.map(normaliseAttachmentExtension));
}

function rejectByType(file: File, allowed: ReadonlySet<string> | undefined): string | null {
  if (allowed === undefined) return null;
  const extension = attachmentExtension(file.name);
  if (extension === '') return `"${file.name}" has no file extension, so it cannot be attached.`;
  if (!allowed.has(extension)) return `"${file.name}" is not a supported file type.`;
  return null;
}

export interface RemainingAttachmentCapacity {
  readonly remainingAttachments: number;
  readonly isAtMaxCapacity: boolean;
  readonly isAtMaxSize: boolean;
}

export function getRemainingAttachmentCapacity(
  attachments: readonly File[],
  limits: Pick<typeof ATTACHMENT_LIMITS, 'MAX_ATTACHMENTS' | 'MAX_TOTAL_SIZE'> = ATTACHMENT_LIMITS,
): RemainingAttachmentCapacity {
  const totalSize = attachments.reduce((sum, file) => sum + file.size, 0);
  return {
    remainingAttachments: Math.max(0, limits.MAX_ATTACHMENTS - attachments.length),
    isAtMaxCapacity: attachments.length >= limits.MAX_ATTACHMENTS,
    isAtMaxSize: totalSize >= limits.MAX_TOTAL_SIZE,
  };
}

export interface AttachmentValidationResult {
  readonly validFiles: readonly File[];
  /** One human-readable message per rejected file/limit, in the order encountered. */
  readonly errors: readonly string[];
}

/**
 * Validates newly-picked files against `ATTACHMENT_LIMITS` and whatever is
 * already attached: total count, total size, and per-file size (a lower cap
 * for images), and — since #940 A15 — file TYPE.
 *
 * Baseline `common/attachmentValidationUtils.js` validates the type against a
 * dynamic backend allow-list (`useAllowedFileTypes`/`useAllowedExtensions`).
 * That was a disclosed gap here: this port had no reader for the served list,
 * so nothing checked extensions at all. `allowedExtensions` is that check —
 * it comes from `entities/attachment`'s `useAllowedAttachmentTypes`, which
 * reads `GET /elitea_core/index_types/prompt_lib/{projectId}`'s
 * document/image/code maps (ELITEA-0484, 0486, 0487, 0488).
 *
 * `undefined` means "this caller has no list", NOT "no file is allowed":
 * every file passes the type check, exactly as before. An EMPTY array is a
 * different statement — the deployment served a list and it was empty — and
 * rejects everything, which is the state ELITEA-0489's disabled control
 * renders. The two must not collapse into one value; see `allowedTypes.ts`'s
 * `isServed` for why.
 */
export function validateAttachmentFiles(
  files: readonly File[],
  existingAttachments: readonly File[],
  limits: typeof ATTACHMENT_LIMITS = ATTACHMENT_LIMITS,
  allowedExtensions?: readonly string[],
): AttachmentValidationResult {
  const remainingSlots = Math.max(0, limits.MAX_ATTACHMENTS - existingAttachments.length);
  if (remainingSlots === 0) {
    return { validFiles: [], errors: [`You've reached the ${limits.MAX_ATTACHMENTS}-file limit.`] };
  }

  const allowed = allowedExtensionSet(allowedExtensions);
  const errors: string[] = [];
  const validFiles: File[] = [];
  let imageCount = existingAttachments.filter(isImageFile).length;
  let totalSize = existingAttachments.reduce((sum, file) => sum + file.size, 0);

  for (const file of files) {
    if (validFiles.length >= remainingSlots) {
      errors.push(
        `You've reached the ${limits.MAX_ATTACHMENTS}-file limit. Only the first ${limits.MAX_ATTACHMENTS} will be processed.`,
      );
      break;
    }

    // TYPE first, and before the size checks. A rejected .exe must be
    // reported as the wrong KIND of file, not as one that is too large — the
    // message is what tells the reader whether a smaller copy would work.
    const typeRejection = rejectByType(file, allowed);
    if (typeRejection !== null) {
      errors.push(typeRejection);
      continue;
    }

    const isImage = isImageFile(file);
    const maxFileSize = isImage ? limits.MAX_IMAGE_FILE_SIZE : limits.DEFAULT_MAX_FILE_SIZE;
    if (file.size > maxFileSize) {
      errors.push(`"${file.name}" exceeds the ${formatFileSize(maxFileSize)} limit.`);
      continue;
    }

    if (isImage && imageCount >= limits.MAX_IMAGE_ATTACHMENTS) {
      errors.push(`Maximum ${limits.MAX_IMAGE_ATTACHMENTS} image attachments allowed.`);
      continue;
    }

    if (totalSize + file.size > limits.MAX_TOTAL_SIZE) {
      errors.push(`Total size limit of ${formatFileSize(limits.MAX_TOTAL_SIZE)} would be exceeded.`);
      continue;
    }

    validFiles.push(file);
    totalSize += file.size;
    if (isImage) imageCount += 1;
  }

  return { validFiles, errors };
}
