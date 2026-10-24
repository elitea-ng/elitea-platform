/**
 * MSW handlers for unit S6's chunked-upload transport (spec §5.7 row 2),
 * POST /api/v2/elitea_core/attachments/prompt_lib/{projectId}/{conversationId}
 * (uploadConversationAttachment). The bodies are typed constants (R-M2), not
 * contract examples: the operation has TWO success responses (201 assembled
 * file, 202 chunk received), and orval's generated mock folds them into one
 * `faker.helpers.arrayElement([...])`, so it cannot serve a chosen one. The
 * constants are typed with the generated response types instead.
 *
 * These probes exercise the
 * ONE sanctioned XMLHttpRequest (R-A4) — msw/node's XMLHttpRequestInterceptor
 * patches XMLHttpRequest directly (no Service Worker involved, unlike the
 * browser integration), so it intercepts these XHR calls exactly like fetch,
 * INCLUDING synthesizing upload progress events for request bodies
 * (@mswjs/interceptors XMLHttpRequestController.respondWith:304-330 fires a
 * single loadstart/progress/load/loadend with loaded=total=body length).
 *
 * IMPORTANT environment note: these handlers deliberately NEVER call
 * `request.formData()` / `.text()` / `.arrayBuffer()` on the intercepted
 * request. Under vitest's jsdom environment, `FormData`/`Blob` on
 * `globalThis` are jsdom's own classes, but `@mswjs/interceptors` re-wraps
 * every XHR body into a NODE-NATIVE `Request` (undici) for handler matching
 * (XMLHttpRequestController.ts:675-685, `new FetchRequest(url, {body:
 * rawFormData})`). Node's multipart parser does not recognise a jsdom
 * `FormData`'s entries as valid `File`/`Blob` webidl values and throws when
 * asked to re-parse them (`request.formData()`) — a cross-realm jsdom/undici
 * interoperability gap, not a defect in `upload.ts`. Field-content
 * assertions therefore happen CLIENT-SIDE in upload.test.ts, via
 * `vi.spyOn(FormData.prototype, 'append')`, which is realm-safe. Reading
 * `request.headers` / `.credentials` / `.url` (never the body) is safe and
 * used here for the header/credentials probes.
 */
import { http, HttpResponse } from 'msw';
import type { RequestHandler } from 'msw';
import type {
  ConversationAttachmentChunkReceived,
  ConversationAttachmentUploaded,
  ErrorResponse,
} from '../../../shared/api/generated/model';

/*
 * The statuses are the Go handler's (conversations/attachments.go): 202 for a
 * chunk the server has taken but not assembled, 201 for the assembled file —
 * which is what makes `upload.ts`'s `status >= 200 && status < 300` a tested
 * claim: a 2xx that is not 200.
 */
const CHUNK_IN_PROGRESS: ConversationAttachmentChunkReceived = {
  status: 'chunk_received',
  file_id: '6f1f0f6a-6d3a-4a1a-9f2f-2a1b3c4d5e6f',
  chunk_index: 0,
  total_chunks: 2,
  message: 'Chunk 1/2 received',
};
/** The final chunk's answer: the assembled file (upload.ts reads `parsed[0]`). */
export const CHUNKED_FILE_UPLOADED: ConversationAttachmentUploaded[] = [
  { filepath: '/chat-attachments/3f7c1e8a-9b2d-4c5e-8a1f-0d6b7c8e9f10/demo.bin', file_size: 5242881 },
];
/** A single-request (file.size <= CHUNK_SIZE) upload's answer. */
export const SMALL_FILE_UPLOADED: ConversationAttachmentUploaded[] = [
  { filepath: '/chat-attachments/3f7c1e8a-9b2d-4c5e-8a1f-0d6b7c8e9f10/small.txt', file_size: 42 },
];
/** apierr.Write's flat shape. */
const SERVER_ERROR: ErrorResponse = { error: 'internal error' };

const UPLOAD_PATH = '*/elitea_core/attachments/prompt_lib/*';

/** Request facts captured for assertions (headers/credentials, §5.7 row 2 probes) — body NEVER read, see module doc. */
export interface CapturedUploadRequest {
  url: string;
  method: string;
  credentials: RequestCredentials;
  authorization: string | null;
  cacheControl: string | null;
  contentType: string | null;
}

function capture(request: Request, sink?: CapturedUploadRequest[]): void {
  if (sink === undefined) return;
  sink.push({
    url: request.url,
    method: request.method,
    credentials: request.credentials,
    authorization: request.headers.get('authorization'),
    cacheControl: request.headers.get('cache-control'),
    contentType: request.headers.get('content-type'),
  });
}

/** Always responds with the in-progress ack — pair with chunkAckComplete() via sequential server.use() calls to model a real chunk sequence. */
export function chunkAckInProgress(sink?: CapturedUploadRequest[]): RequestHandler {
  return http.post(UPLOAD_PATH, ({ request }) => {
    capture(request, sink);
    return HttpResponse.json(CHUNK_IN_PROGRESS, { status: 202 });
  });
}

/** Always responds with the final-chunk ack. */
export function chunkAckComplete(sink?: CapturedUploadRequest[]): RequestHandler {
  return http.post(UPLOAD_PATH, ({ request }) => {
    capture(request, sink);
    return HttpResponse.json(CHUNKED_FILE_UPLOADED, { status: 201 });
  });
}

export function smallFileOk(sink?: CapturedUploadRequest[]): RequestHandler {
  return http.post(UPLOAD_PATH, ({ request }) => {
    capture(request, sink);
    return HttpResponse.json(SMALL_FILE_UPLOADED, { status: 201 });
  });
}

export function uploadServerError(): RequestHandler {
  return http.post(UPLOAD_PATH, () => HttpResponse.json(SERVER_ERROR, { status: 500 }));
}

export function uploadNetworkError(): RequestHandler {
  return http.post(UPLOAD_PATH, () => HttpResponse.error());
}

/**
 * Responds in_progress for every call except the `finalCallNumber`-th
 * (1-based), which gets the complete ack. Call-count based (never reads the
 * body, see module doc) — models a real multi-chunk sequence for
 * `uploadFileWithProgress`, whose chunk loop awaits each POST sequentially
 * so call order == chunk order.
 */
export function chunkAckSequence(finalCallNumber: number, sink?: CapturedUploadRequest[]): RequestHandler {
  let callCount = 0;
  return http.post(UPLOAD_PATH, ({ request }) => {
    callCount += 1;
    capture(request, sink);
    return callCount === finalCallNumber
      ? HttpResponse.json(CHUNKED_FILE_UPLOADED, { status: 201 })
      : HttpResponse.json(CHUNK_IN_PROGRESS, { status: 202 });
  });
}
