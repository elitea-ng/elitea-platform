/**
 * The desktop's `fetch`: a request the Tauri host sends on its ONE pooled
 * HTTP client (`apps/elitea-desktop/src-tauri/src/net.rs`, IPC.md "Network").
 *
 * It replaces `@tauri-apps/plugin-http`, which built a new client for every
 * request: a TLS root-store load (85–470 ms, serialised across the process)
 * and a fresh TCP + TLS connection each time. The host keeps the rules the
 * plugin had here — the connected deployment's origin only, redirects never
 * followed, no cookies — and the body streams one chunk per read, so the
 * fetch-based SSE (`fetchEventSource.ts`) works unchanged.
 *
 * Wire format of `http_fetch`: one binary IPC body, a 4-byte big-endian
 * length, that many bytes of JSON `{id, method, url, headers}`, then the
 * request body. The id is the page's, so an abort can name a request before
 * its answer arrives (the host remembers an abort that overtakes its request).
 *
 * Bounds: a request body over {@link MAX_REQUEST_BODY_BYTES} is refused here,
 * before it is buffered when its size is known. A response body the page can
 * no longer read — its stream was garbage-collected unread — is released on
 * the host, and a HEAD or null-body response holds nothing there; the host
 * also drops a body nobody reads for a minute.
 */

/** The `invoke` of `window.__TAURI_INTERNALS__`: a typed array argument is sent as a raw body. */
export type RawInvoke = (command: string, args?: unknown) => Promise<unknown>;

interface FetchHead {
  status: number;
  statusText: string;
  headers: [string, string][];
  url: string;
  hasBody: boolean;
}

/** What the host rejects with (`FetchError` in net.rs). */
interface HostFetchError {
  code: string;
  message: string;
}

function isHostFetchError(value: unknown): value is HostFetchError {
  return typeof value === 'object' && value !== null && typeof (value as { code?: unknown }).code === 'string';
}

/** Read live: an abort can land while the host call is awaited. */
function aborted(signal: AbortSignal | null | undefined): boolean {
  return signal?.aborted === true;
}

function abortError(): DOMException {
  return new DOMException('The operation was aborted.', 'AbortError');
}

/** As fetch fails: `AbortError` for an abort, a `TypeError` for everything else. */
function toFetchError(cause: unknown, signal: AbortSignal | null | undefined): Error {
  if (aborted(signal) || (isHostFetchError(cause) && cause.code === 'aborted')) return abortError();
  if (isHostFetchError(cause)) return new TypeError(cause.message);
  if (cause instanceof Error) return cause;
  return new TypeError(typeof cause === 'string' ? cause : 'Load failed');
}

/**
 * The largest request body the desktop sends: the host's `MAX_REQUEST_BODY`
 * (net.rs). The largest single-request upload is an artifact, whose
 * deployment default is 150 MiB; this leaves room for its multipart envelope.
 */
export const MAX_REQUEST_BODY_BYTES = 160 * 1024 * 1024;

/** Statuses whose response has no body (fetch spec "null body status"). */
const NULL_BODY_STATUSES = new Set([101, 103, 204, 205, 304]);

/**
 * A lower bound of `body`'s size in bytes, known without reading it, or
 * `undefined` when it cannot be told before buffering (a stream). A string's
 * UTF-16 length never exceeds its UTF-8 byte length.
 */
function knownBodySize(body: BodyInit | null | undefined): number | undefined {
  if (body === null || body === undefined) return undefined;
  if (typeof body === 'string') return body.length;
  if (body instanceof Blob) return body.size;
  if (body instanceof ArrayBuffer) return body.byteLength;
  if (ArrayBuffer.isView(body)) return body.byteLength;
  if (body instanceof URLSearchParams) return body.toString().length;
  if (body instanceof FormData) {
    let size = 0;
    for (const [name, value] of body) size += name.length + (typeof value === 'string' ? value.length : value.size);
    return size;
  }
  return undefined;
}

function tooLarge(limit: number): TypeError {
  return new TypeError(`The request body is larger than the desktop app can send (${String(Math.floor(limit / (1024 * 1024)))} MiB).`);
}

/** The part of `FinalizationRegistry` this module uses (tests pass a fake). */
export interface ReleaseRegistry {
  register(target: object, id: number, token: object): void;
  unregister(token: object): void;
}

/** As fetch: the init's signal, else the input Request's own. */
function requestSignal(input: RequestInfo | URL, init: RequestInit | undefined): AbortSignal | null {
  return init?.signal ?? (input instanceof Request ? input.signal : null);
}

/** The request's bytes, refused over `limit`: before buffering when the size is known, after it otherwise. */
async function boundedBody(request: Request, init: RequestInit | undefined, limit: number): Promise<Uint8Array> {
  if ((knownBodySize(init?.body) ?? 0) > limit) throw tooLarge(limit);
  const body = new Uint8Array(await request.arrayBuffer());
  if (body.byteLength > limit) throw tooLarge(limit);
  return body;
}

/** Whether there is a body to read: never for HEAD or a null-body status, whatever the host said. */
function readableBody(head: FetchHead, method: string): boolean {
  return head.hasBody && method !== 'HEAD' && !NULL_BODY_STATUSES.has(head.status);
}

/** The response body: one `http_read_body` per pull; `settled` once it ends, `cancel` when the reader cancels. */
function hostBodyStream(
  invoke: RawInvoke,
  id: number,
  signal: AbortSignal | null,
  settled: () => void,
  cancel: () => void,
): ReadableStream<Uint8Array> {
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const chunk = new Uint8Array((await invoke('http_read_body', { id })) as ArrayBuffer);
        if (chunk.byteLength === 0) {
          settled();
          controller.close();
        } else {
          controller.enqueue(chunk);
        }
      } catch (cause) {
        settled();
        controller.error(toFetchError(cause, signal));
      }
    },
    cancel() {
      settled();
      cancel();
    },
  });
}

/** Frame metadata and body into the one buffer `http_fetch` reads. */
export function encodeFetchFrame(meta: object, body: Uint8Array): Uint8Array {
  const json = new TextEncoder().encode(JSON.stringify(meta));
  const frame = new Uint8Array(4 + json.byteLength + body.byteLength);
  new DataView(frame.buffer).setUint32(0, json.byteLength);
  frame.set(json, 4);
  frame.set(body, 4 + json.byteLength);
  return frame;
}

export interface HostFetchOptions {
  /** The first request id (tests); default a random one, so ids of a reloaded page do not meet the last page's. */
  firstId?: number;
  /** The request body bound (tests); default {@link MAX_REQUEST_BODY_BYTES}. */
  maxBodyBytes?: number;
  /** How an unread body is released once collected (tests); default a `FinalizationRegistry`. */
  createRegistry?: (release: (id: number) => void) => ReleaseRegistry | undefined;
}

function finalizationRegistry(release: (id: number) => void): ReleaseRegistry | undefined {
  return typeof FinalizationRegistry === 'function' ? new FinalizationRegistry<number>(release) : undefined;
}

export function createHostFetch(invoke: RawInvoke, options: HostFetchOptions = {}): typeof fetch {
  let nextId = options.firstId ?? Math.floor(Math.random() * 2 ** 31);
  const maxBody = options.maxBodyBytes ?? MAX_REQUEST_BODY_BYTES;
  const release = (id: number): void => {
    void invoke('http_cancel', { id }).catch(() => undefined);
  };
  // A body stream collected before it was read to its end: nobody can read
  // it any more, so the host lets the response go.
  const registry = (options.createRegistry ?? finalizationRegistry)(release);

  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    // `Request` does the normalising fetch would: the body's bytes and its
    // Content-Type (a FormData boundary), the method, and the headers a page may set.
    const request = new Request(input, init);
    const signal = requestSignal(input, init);
    if (aborted(signal)) throw abortError();
    const body = await boundedBody(request, init, maxBody);
    nextId += 1;
    const id = nextId;
    const cancel = (): void => release(id);
    const token = {};
    signal?.addEventListener('abort', cancel, { once: true });
    const settled = (): void => {
      signal?.removeEventListener('abort', cancel);
      registry?.unregister(token);
    };

    let head: FetchHead;
    try {
      head = (await invoke('http_fetch', encodeFetchFrame({ id, method: request.method, url: request.url, headers: [...request.headers] }, body))) as FetchHead;
    } catch (cause) {
      settled();
      throw toFetchError(cause, signal);
    }
    if (aborted(signal)) {
      cancel();
      throw abortError();
    }
    // HEAD and a null-body status have nothing to read: release the host's
    // entry at once, whatever the host answered.
    const hasBody = readableBody(head, request.method);
    if (head.hasBody && !hasBody) cancel();
    if (!hasBody) settled();

    const stream = hasBody ? hostBodyStream(invoke, id, signal, settled, cancel) : null;
    if (stream !== null) registry?.register(stream, id, token);
    const response = new Response(stream, { status: head.status, statusText: head.statusText, headers: head.headers });
    Object.defineProperty(response, 'url', { value: head.url });
    return response;
  };
}
