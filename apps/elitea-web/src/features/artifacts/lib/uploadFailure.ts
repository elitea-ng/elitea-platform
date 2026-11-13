import type { HttpFailure } from '@/shared/api/http';
import { productName } from '@/shared/brand';

/** The message of the server's own JSON error envelope, if the body is one. */
export function envelopeMessage(body: unknown): string | undefined {
  if (typeof body !== 'object' || body === null) return undefined;
  const error = (body as { error?: unknown }).error;
  if (typeof error === 'string' && error !== '') return error;
  if (typeof error === 'object' && error !== null) {
    const message = (error as { message?: unknown }).message;
    if (typeof message === 'string' && message !== '') return message;
  }
  const message = (body as { message?: unknown }).message;
  return typeof message === 'string' && message !== '' ? message : undefined;
}

/** An HTML page, as a WAF or proxy answers: the body is text that opens a tag. */
function isHtmlBody(body: unknown): boolean {
  return typeof body === 'string' && body.trimStart().startsWith('<');
}

/**
 * Why one upload failed, in words the user can act on (#2004). Only an HTML
 * answer (a WAF or proxy block page in front of the deployment) is named as
 * "blocked before it reached" the product; a plain-text or empty 413/500/502
 * may well have come from the server itself, so it says only that the server
 * refused the upload, with the status.
 */
export function describeUploadFailure(failure: HttpFailure): string {
  if (failure.kind === 'http') {
    const message = envelopeMessage(failure.body);
    if (message !== undefined) return message;
    if (isHtmlBody(failure.body)) {
      return `The upload was blocked before it reached ${productName()} (HTTP ${failure.status} from a proxy). Ask your administrator.`;
    }
    return `The server refused the upload (HTTP ${failure.status}).`;
  }
  if (failure.kind === 'network') return 'The connection failed before the upload finished.';
  if (failure.kind === 'aborted') return 'The upload was cancelled.';
  return 'You are not signed in or your session expired.';
}
