import { randomUUID } from 'expo-crypto';
import { Directory, File, Paths } from 'expo-file-system';
import { z } from 'zod';

import { createApiClient, type ApiClient } from '@/api/client';
import { isApiError } from '@/api/errors';
import { getSession } from '@/state/session';

/**
 * Chat attachment upload (`uploadConversationAttachment`), through the
 * guarded fetch like every other call: the origin guard, the bearer header
 * (never in a URL), X-Client-Version, refresh-on-401 and 426 all apply.
 * expo-file-system's own upload tasks are NOT used, because they bypass all
 * of that.
 *
 * A file up to `chunk_bytes` goes in one multipart request (201 with its
 * `filepath`). A larger one goes in chunks read at an offset from disk (never
 * the whole file in memory, never base64): each chunk is written to a
 * temporary file in the cache and sent as one multipart request with
 * `file_id`, `chunk_index` and `total_chunks`; the server answers 202 until
 * the last, then 201. A chunk is retried up to 3 times (the server upserts
 * it). Temporary files are deleted whatever happens. One file at a time.
 */

export const UPLOAD_TIMEOUT_MS = 120_000;
export const CHUNK_RETRIES = 3;
const FILE_ID_RE = /^[A-Za-z0-9_-]{1,64}$/;

export interface UploadSource {
  /** Local file to send. */
  uri: string;
  /** The name it is stored under (already sanitised and suffixed). */
  uploadName: string;
  size: number;
  mimeType: string;
}

export interface Uploaded {
  /** Exactly what the send's `payload.attachments[].filepath` must carry. */
  filepath: string;
  size: number;
}

export type UploadFailure = 'refused' | 'too_large' | 'not_allowed' | 'unreachable' | 'bad_response' | 'session' | 'upgrade_required' | 'cancelled';

export class UploadError extends Error {
  constructor(
    readonly reason: UploadFailure,
    readonly serverMessage: string | null = null,
  ) {
    super(`upload failed: ${reason}`);
    this.name = 'UploadError';
  }
}

export function uploadErrorText(error: unknown, name: string): string {
  if (!(error instanceof UploadError)) return `${name} could not be uploaded. Try again.`;
  switch (error.reason) {
    case 'too_large':
      return error.serverMessage ? `${name}: ${error.serverMessage}` : `${name} is larger than this workspace allows.`;
    case 'refused':
      return error.serverMessage ? `${name}: ${error.serverMessage}` : `The workspace refused ${name}.`;
    case 'not_allowed':
      return "You can't attach files in this conversation.";
    case 'unreachable':
      return `${name} could not be uploaded: the workspace did not answer. Try again.`;
    case 'session':
      return 'You were signed out.';
    case 'upgrade_required':
      return 'Update Agent Zefir to attach files.';
    case 'cancelled':
      return `The upload of ${name} was cancelled.`;
    default:
      return `${name} could not be uploaded. Try again.`;
  }
}

/** The bits of the platform the upload needs (tests inject fakes). */
export interface UploadDeps {
  client?: ApiClient;
  /** The multipart file part for a local file. */
  part?: (uri: string, name: string, type: string) => unknown;
  /** Reads `length` bytes at `offset` of a local file. */
  readChunk?: (uri: string, offset: number, length: number) => Promise<Uint8Array>;
  /** Writes bytes to a new temporary file; returns its uri and a deleter. */
  writeTemp?: (bytes: Uint8Array) => { uri: string; delete: () => void };
  newFileId?: () => string;
  sleep?: (ms: number) => Promise<void>;
  onProgress?: (fraction: number | null) => void;
  /** Cancels the upload: the request in flight is aborted and no further chunk is sent. */
  signal?: AbortSignal;
}

/**
 * The multipart file part for a local file, as the app's fetch can send it.
 *
 * Expo installs `expo/fetch` as the global fetch, and its multipart encoder
 * accepts strings, Blobs and objects with `bytes()` — it refuses React
 * Native's `{uri, name, type}` part ("Unsupported FormDataPart
 * implementation") before anything is sent. So the part reads the file when
 * the request is encoded, and carries the upload name and type itself (the
 * stored file's basename is a cache name, not the name the user sees). Single
 * shots are at most `chunk_bytes`, as is every chunk's temporary file, so one
 * part never holds more than a chunk in memory.
 */
export const nativePart = (uri: string, name: string, type: string): unknown => ({
  name,
  type,
  bytes: () => new File(uri).bytes(),
});

async function nativeReadChunk(uri: string, offset: number, length: number): Promise<Uint8Array> {
  const handle = new File(uri).open();
  try {
    handle.offset = offset;
    return await handle.readBytes(length);
  } finally {
    handle.close();
  }
}

/** Where upload chunks are staged: swept on start and with the session's caches. */
export const UPLOAD_TEMP_DIR = 'att-up';

function nativeWriteTemp(bytes: Uint8Array): { uri: string; delete: () => void } {
  const dir = new Directory(Paths.cache, UPLOAD_TEMP_DIR);
  if (!dir.exists) dir.create({ intermediates: true });
  const file = new File(dir, `${randomUUID()}.part`);
  file.write(bytes);
  return {
    uri: file.uri,
    delete: () => {
      try {
        if (file.exists) file.delete();
      } catch {
        // swept later
      }
    },
  };
}

function uploadClient(): ApiClient {
  const origin = getSession().serverOrigin;
  if (origin === null) throw new UploadError('session');
  // A longer deadline than the 20 s REST default: a 5 MiB chunk on a phone.
  return createApiClient({ origin, auth: { kind: 'session' }, timeoutMs: UPLOAD_TIMEOUT_MS });
}

const Uploaded201 = z.array(z.looseObject({ filepath: z.string().min(1).max(1024), file_size: z.number().nullish() })).min(1);
const ErrorBody = z.looseObject({ error: z.string().max(1000).nullish(), message: z.string().max(1000).nullish() });

type PostOutcome = { kind: 'stored'; uploaded: Uploaded } | { kind: 'chunk_received' };

function classify(status: number, body: unknown): UploadError {
  const parsed = ErrorBody.safeParse(body);
  const message = parsed.success ? (parsed.data.error ?? parsed.data.message ?? null) : null;
  if (status === 403) return new UploadError('not_allowed', message);
  if (status === 413 || (status === 400 && message !== null && /exceed|too large|limit/i.test(message))) {
    return new UploadError('too_large', message);
  }
  if (status >= 500 || status === 429) return new UploadError('unreachable', message);
  return new UploadError('refused', message);
}

async function post(
  client: ApiClient,
  projectId: number,
  conversationUuid: string,
  form: FormData,
  signal: AbortSignal | undefined,
): Promise<PostOutcome> {
  if (signal?.aborted) throw new UploadError('cancelled');
  let result;
  try {
    result = await client.POST('/elitea_core/attachments/prompt_lib/{project_id}/{conversation_id}', {
      signal,
      params: { path: { project_id: String(projectId), conversation_id: conversationUuid } },
      // FormData as is: the platform sets the multipart boundary itself.
      body: form as never,
      bodySerializer: (body: unknown) => body as FormData,
    });
  } catch (error) {
    if (signal?.aborted || (isApiError(error) && error.kind === 'aborted')) throw new UploadError('cancelled');
    if (isApiError(error) && (error.kind === 'network' || error.kind === 'timeout')) throw new UploadError('unreachable');
    if (isApiError(error) && error.kind === 'upgrade_required') throw new UploadError('upgrade_required');
    if (isApiError(error)) throw new UploadError('session');
    throw error;
  }
  const { data, error, response } = result;
  if (response.status === 201) {
    const parsed = Uploaded201.safeParse(data);
    if (!parsed.success) throw new UploadError('bad_response');
    return { kind: 'stored', uploaded: { filepath: parsed.data[0].filepath, size: parsed.data[0].file_size ?? 0 } };
  }
  if (response.status === 202) return { kind: 'chunk_received' };
  throw classify(response.status, error ?? data);
}

/**
 * Uploads one prepared file into the conversation's attachments and
 * resolves to its `filepath`. Throws `UploadError`.
 */
export async function uploadAttachment(
  projectId: number,
  conversationUuid: string,
  source: UploadSource,
  chunkBytes: number,
  deps: UploadDeps = {},
): Promise<Uploaded> {
  if (!/^[0-9a-fA-F-]{36}$/.test(conversationUuid)) throw new UploadError('refused');
  const client = deps.client ?? uploadClient();
  const part = deps.part ?? nativePart;
  const sleep = deps.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));

  if (source.size <= chunkBytes) {
    deps.onProgress?.(null);
    const form = new FormData();
    form.append('file', part(source.uri, source.uploadName, source.mimeType) as never);
    form.append('file_name', source.uploadName);
    const outcome = await post(client, projectId, conversationUuid, form, deps.signal);
    if (outcome.kind !== 'stored') throw new UploadError('bad_response');
    deps.onProgress?.(1);
    return outcome.uploaded;
  }

  const readChunk = deps.readChunk ?? nativeReadChunk;
  const writeTemp = deps.writeTemp ?? nativeWriteTemp;
  const fileId = (deps.newFileId ?? (() => randomUUID().replace(/-/g, '')))();
  if (!FILE_ID_RE.test(fileId)) throw new UploadError('bad_response');
  const total = Math.ceil(source.size / chunkBytes);
  deps.onProgress?.(0);

  for (let index = 0; index < total; index++) {
    if (deps.signal?.aborted) throw new UploadError('cancelled');
    const offset = index * chunkBytes;
    const length = Math.min(chunkBytes, source.size - offset);
    const bytes = await readChunk(source.uri, offset, length);
    if (bytes.byteLength !== length) throw new UploadError('refused', 'The file changed while it was being uploaded.');
    const temp = writeTemp(bytes);
    try {
      let outcome: PostOutcome | null = null;
      for (let attempt = 0; ; attempt++) {
        const form = new FormData();
        form.append('file', part(temp.uri, source.uploadName, source.mimeType) as never);
        form.append('file_name', source.uploadName);
        form.append('file_id', fileId);
        form.append('chunk_index', String(index));
        form.append('total_chunks', String(total));
        try {
          outcome = await post(client, projectId, conversationUuid, form, deps.signal);
          break;
        } catch (error) {
          if (!(error instanceof UploadError) || error.reason !== 'unreachable' || attempt + 1 >= CHUNK_RETRIES || deps.signal?.aborted) throw error;
          await sleep(500 * 2 ** attempt);
        }
      }
      const last = index === total - 1;
      if (last) {
        if (outcome.kind !== 'stored') throw new UploadError('bad_response');
        deps.onProgress?.(1);
        return outcome.uploaded;
      }
      if (outcome.kind !== 'chunk_received') throw new UploadError('bad_response');
      deps.onProgress?.((index + 1) / total);
    } finally {
      temp.delete();
    }
  }
  throw new UploadError('bad_response');
}

/** Deletes upload chunks left by a crash (app start, session wipe). */
export function sweepUploadTemp(): void {
  try {
    const dir = new Directory(Paths.cache, UPLOAD_TEMP_DIR);
    if (dir.exists) dir.delete();
  } catch {
    // best effort
  }
}
