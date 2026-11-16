import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

/** What a caller's server stores: allowed extensions (lower case, with the dot) and the largest upload in bytes. */
export interface UploadLimits {
  readonly extensions?: readonly string[] | undefined;
  readonly maxBytes?: number | undefined;
}

/** The upload's measured (and capped) pixel box. */
export interface UploadDimensions {
  readonly width: number;
  readonly height: number;
}

/** The maximum icon edge the server stores; the client measures against it. */
export const MAX_ICON_EDGE = 64;

const DEFAULT_EXTENSIONS: readonly string[] = ['.jpg', '.jpeg', '.png', '.tiff', '.webp', '.gif', '.bmp', '.ico'];

function fileExtension(name: string): string {
  const dot = name.lastIndexOf('.');
  return dot < 0 ? '' : name.slice(dot).toLowerCase();
}

/** The reason an upload cannot be sent (#2107), or `undefined` when it can. */
export function uploadProblem(file: File, extensions: readonly string[], maxFileBytes: number | undefined): string | undefined {
  if (!extensions.includes(fileExtension(file.name))) {
    const formats = extensions.map((extension) => extension.slice(1).toUpperCase()).join(', ');
    return t('shared.iconPicker.errorFormat', 'Unsupported file format. Upload one of: {{formats}}.', { formats });
  }
  if (maxFileBytes !== undefined && file.size > maxFileBytes) {
    const limit = `${Math.round(maxFileBytes / 1024)} KB`;
    return t('shared.iconPicker.errorSize', 'File size exceeds {{limit}}.', { limit });
  }
  return undefined;
}

/** The file itself could not be read or decoded in the browser, before any upload. */
class UnreadableImageError extends Error {}

/**
 * What the dialog says about a failure. The server's own sentence for a refused
 * upload; a specific line for a file the browser cannot decode; a network-shaped
 * line for a failure that never reached the server; else a generic one. This
 * dialog is the only place the failure is shown: no mutation toasts it.
 */
export function uploadFailureMessage(error: unknown): string {
  if (error instanceof UnreadableImageError) {
    return t('shared.iconPicker.errorUnreadable', 'The image could not be read. It may be corrupted or not a real image file.');
  }
  if (!(error instanceof EliteaApiError)) {
    return t('shared.iconPicker.errorNetwork', 'The icon could not be uploaded. Check your connection and try again.');
  }
  if (error.failure.kind === 'http') {
    const body = error.failure.body;
    if (typeof body === 'object' && body !== null) {
      const message = (body as { error?: unknown; msg?: unknown }).error ?? (body as { msg?: unknown }).msg;
      if (typeof message === 'string' && message !== '') return message;
    }
  }
  return t('shared.iconPicker.errorUpload', 'The icon could not be uploaded.');
}

/**
 * Decodes `file` through `Image()` to get its real (capped) pixel dimensions.
 * Never called for TIFF — browsers cannot decode TIFF via `Image()`, so
 * `image.onload` would never fire and the upload would hang forever.
 */
export function readImageDimensions(file: File): Promise<UploadDimensions> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = (event) => {
      const image = new Image();
      image.onload = () => {
        resolve({
          width: Math.min(image.width, MAX_ICON_EDGE),
          height: Math.min(image.height, MAX_ICON_EDGE),
        });
      };
      image.onerror = () => { reject(new UnreadableImageError(`icon picker: failed to decode image "${file.name}"`)); };
      image.src = (event.target?.result as string | null) ?? '';
    };
    reader.onerror = () => { reject(new UnreadableImageError(`icon picker: failed to read file "${file.name}"`)); };
    reader.readAsDataURL(file);
  });
}

/** The limits in force: the caller's, with the browser-drawable extension set as the default. */
export function resolveUploadLimits(limits: UploadLimits | undefined): { readonly extensions: readonly string[]; readonly maxBytes: number | undefined } {
  return { extensions: limits?.extensions ?? DEFAULT_EXTENSIONS, maxBytes: limits?.maxBytes };
}
