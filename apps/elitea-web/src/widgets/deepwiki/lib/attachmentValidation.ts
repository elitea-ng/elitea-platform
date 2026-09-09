/**
 * Validation for the wiki chat's file attachments (#873).
 *
 * SMALL TEXT FILES ONLY, and this is what enforces it before a single byte is
 * read: the engine folds an attachment's content straight into the question
 * (`services/elitea-subapp-host/internal/apps/deepwiki/run/extracontext.go`),
 * which truncates at 6,000 characters PER FILE and 16,000 total — so a large
 * binary would either be refused here for its extension, or read, uploaded
 * and mostly thrown away server-side. Refusing early is the honest answer:
 * a file this control cannot usefully send should not appear to be attached.
 *
 * THE CAP is `MAX_ATTACHMENTS` (5), matching the engine's own
 * `MaxExtraContextFiles` — a client that lets the reader attach a 6th file
 * only to have the server refuse the whole question is a worse experience
 * than not offering the slot at all.
 */

/** Extensions this control accepts, matched case-insensitively. */
export const ALLOWED_ATTACHMENT_EXTENSIONS = [
  '.md',
  '.mdx',
  '.txt',
  '.csv',
  '.json',
  '.yaml',
  '.yml',
  '.toml',
  '.ini',
  '.env',
  '.log',
  '.py',
  '.js',
  '.jsx',
  '.ts',
  '.tsx',
  '.go',
  '.java',
  '.rb',
  '.rs',
  '.c',
  '.h',
  '.cpp',
  '.hpp',
  '.cs',
  '.php',
  '.sh',
  '.sql',
  '.xml',
  '.html',
  '.css',
] as const;

/** Matches the engine's own `MaxExtraContextFiles`. */
export const MAX_ATTACHMENTS = 5;

/**
 * The reading budget, in bytes. Well above what the engine keeps (6,000
 * characters per file) — the point of this cap is not to bound what the
 * question carries, the engine already does that, it is to stop the browser
 * from reading a multi-megabyte file into memory just to throw it away.
 */
export const MAX_ATTACHMENT_FILE_BYTES = 200 * 1024;

export interface AttachmentValidationResult {
  readonly validFiles: readonly File[];
  /** One human-readable message per rejected file/limit, in order. */
  readonly errors: readonly string[];
}

function hasAllowedExtension(name: string): boolean {
  const lower = name.toLowerCase();
  return ALLOWED_ATTACHMENT_EXTENSIONS.some((extension) => lower.endsWith(extension));
}

/**
 * Validate newly-picked files against the count cap, the per-file size cap
 * and the extension allowlist, given what is already attached.
 */
export function validateWikiAttachmentFiles(
  files: readonly File[],
  existing: readonly { readonly name: string }[],
): AttachmentValidationResult {
  const remainingSlots = Math.max(0, MAX_ATTACHMENTS - existing.length);
  if (remainingSlots === 0) {
    return { validFiles: [], errors: [`You can attach at most ${String(MAX_ATTACHMENTS)} files.`] };
  }

  const errors: string[] = [];
  const validFiles: File[] = [];

  for (const file of files) {
    if (validFiles.length >= remainingSlots) {
      errors.push(`You can attach at most ${String(MAX_ATTACHMENTS)} files; the rest were skipped.`);
      break;
    }
    if (!hasAllowedExtension(file.name)) {
      errors.push(`"${file.name}" is not a supported text file type.`);
      continue;
    }
    if (file.size > MAX_ATTACHMENT_FILE_BYTES) {
      errors.push(`"${file.name}" is larger than ${String(Math.round(MAX_ATTACHMENT_FILE_BYTES / 1024))}KB.`);
      continue;
    }
    validFiles.push(file);
  }

  return { validFiles, errors };
}
