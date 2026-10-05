import { createApiClient, type ApiClient } from '@/api/client';
import { isApiError } from '@/api/errors';
import { getSession } from '@/state/session';

/**
 * Chat attachment download (`downloadConversationAttachment`, client
 * contract 1.1), through the guarded fetch: same origin only (a redirect to
 * another origin is refused unread), bearer in a header, never in a URL.
 * The server answers `Cache-Control: private, no-store`, `nosniff` and
 * `Content-Disposition: attachment`, and serves active types (HTML, SVG,
 * XML, script) as `application/octet-stream`; the app never renders those
 * itself either.
 *
 * The bytes are held in memory for the one write to a temporary file, so a
 * file larger than `MAX_DOWNLOAD_BYTES` is refused: by its Content-Length
 * before reading, else as soon as the bytes read pass the limit.
 */

export const MAX_DOWNLOAD_BYTES = 50 * 1024 * 1024;
export const DOWNLOAD_TIMEOUT_MS = 120_000;

export type DownloadFailure = 'not_allowed' | 'not_found' | 'not_available' | 'too_large' | 'unreachable' | 'session' | 'failed';

export class DownloadError extends Error {
  constructor(readonly reason: DownloadFailure) {
    super(`download failed: ${reason}`);
    this.name = 'DownloadError';
  }
}

export function downloadErrorText(error: unknown): string {
  if (!(error instanceof DownloadError)) return "This file couldn't be opened.";
  switch (error.reason) {
    case 'not_allowed':
      return "You don't have access to download this file.";
    case 'not_found':
      return 'This file is no longer available.';
    case 'not_available':
      return 'This workspace does not offer file downloads to the app yet.';
    case 'too_large':
      return 'This file is too large to open on this device.';
    case 'unreachable':
      return "The workspace didn't answer. Try again.";
    case 'session':
      return 'You were signed out.';
    default:
      return "This file couldn't be opened.";
  }
}

/** A stored name the route takes: one segment, no `..`, no slashes or control characters. */
export function isSafeStoredName(name: string): boolean {
  return name !== '' && name !== '.' && name !== '..' && name.length <= 255 && !/[/\\\u0000-\u001f\u007f]/.test(name);
}

export interface Downloaded {
  bytes: Uint8Array;
  /** The type the server served (octet-stream for anything active). */
  contentType: string;
}

function downloadClient(): ApiClient {
  const origin = getSession().serverOrigin;
  if (origin === null) throw new DownloadError('session');
  return createApiClient({ origin, auth: { kind: 'session' }, timeoutMs: DOWNLOAD_TIMEOUT_MS });
}

export async function downloadAttachment(
  projectId: number,
  conversationUuid: string,
  storedName: string,
  client: ApiClient = downloadClient(),
): Promise<Downloaded> {
  if (!/^[0-9a-fA-F-]{36}$/.test(conversationUuid) || !isSafeStoredName(storedName)) throw new DownloadError('not_found');
  let result;
  try {
    result = await client.GET('/elitea_core/attachments/prompt_lib/{project_id}/{conversation_id}/{name}', {
      params: { path: { project_id: String(projectId), conversation_id: conversationUuid, name: storedName } },
      parseAs: 'stream',
    });
  } catch (error) {
    if (isApiError(error) && (error.kind === 'network' || error.kind === 'timeout')) throw new DownloadError('unreachable');
    if (isApiError(error)) throw new DownloadError('session');
    throw error;
  }
  const { response } = result;
  if (!response.ok) {
    // Always drain or cancel the body; nothing of an error body is kept.
    await response.body?.cancel().catch(() => {});
    if (response.status === 403) throw new DownloadError('not_allowed');
    if (response.status === 404) throw new DownloadError('not_found');
    if (response.status === 501) throw new DownloadError('not_available');
    if (response.status >= 500 || response.status === 429) throw new DownloadError('unreachable');
    throw new DownloadError('failed');
  }
  // A missing Content-Length is "unknown", not 0: the body is counted as it arrives.
  const header = response.headers.get('Content-Length');
  const declared = header === null ? NaN : Number(header);
  if (Number.isFinite(declared) && declared > MAX_DOWNLOAD_BYTES) {
    await response.body?.cancel().catch(() => {});
    throw new DownloadError('too_large');
  }
  return {
    bytes: await readCapped(response),
    contentType: (response.headers.get('Content-Type') ?? 'application/octet-stream').split(';')[0].trim().toLowerCase(),
  };
}

/**
 * The body, read piece by piece and given up (cancelled) as soon as it passes
 * `MAX_DOWNLOAD_BYTES`: a chunked or compressed answer with no length is
 * never buffered whole before it is refused.
 */
async function readCapped(response: Response): Promise<Uint8Array> {
  const reader = response.body?.getReader();
  if (!reader) {
    // No stream from this fetch: read it whole, still refused above the limit.
    const buffer = await response.arrayBuffer().catch(() => {
      throw new DownloadError('unreachable');
    });
    if (buffer.byteLength > MAX_DOWNLOAD_BYTES) throw new DownloadError('too_large');
    return new Uint8Array(buffer);
  }
  const parts: Uint8Array[] = [];
  let length = 0;
  for (;;) {
    let next: ReadableStreamReadResult<Uint8Array>;
    try {
      next = await reader.read();
    } catch {
      throw new DownloadError('unreachable');
    }
    if (next.done) break;
    length += next.value.byteLength;
    if (length > MAX_DOWNLOAD_BYTES) {
      await reader.cancel().catch(() => {});
      throw new DownloadError('too_large');
    }
    parts.push(next.value);
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const part of parts) {
    bytes.set(part, offset);
    offset += part.byteLength;
  }
  return bytes;
}
