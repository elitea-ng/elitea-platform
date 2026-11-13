import type { HttpFailure } from '@/shared/api/http';

function envelopeMessage(body: unknown): string | undefined {
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

/**
 * Why one upload failed, in words the user can act on (#2004). A response that
 * is not Elitea's own JSON envelope (an HTML 403 from a WAF or proxy in front of
 * the deployment, say) never reached the server, so it is named as such instead
 * of being collapsed into a bare "Failed to upload".
 */
export function describeUploadFailure(failure: HttpFailure): string {
  if (failure.kind === 'http') {
    const message = envelopeMessage(failure.body);
    if (message !== undefined) return message;
    return `The upload was blocked before it reached Elitea (HTTP ${failure.status} from a proxy). Ask your administrator.`;
  }
  if (failure.kind === 'network') return 'The connection failed before the upload finished.';
  if (failure.kind === 'aborted') return 'The upload was cancelled.';
  return 'You are not signed in or your session expired.';
}
