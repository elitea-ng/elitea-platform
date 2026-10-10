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
 * its answer arrives.
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
}

export function createHostFetch(invoke: RawInvoke, options: HostFetchOptions = {}): typeof fetch {
  let nextId = options.firstId ?? Math.floor(Math.random() * 2 ** 31);

  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    // `Request` does the normalising fetch would: the body's bytes and its
    // Content-Type (a FormData boundary), the method, and the headers a page may set.
    const request = new Request(input, init);
    const signal = init?.signal ?? null;
    if (aborted(signal)) throw abortError();
    const body = new Uint8Array(await request.arrayBuffer());
    nextId += 1;
    const id = nextId;
    const cancel = (): void => {
      void invoke('http_cancel', { id }).catch(() => undefined);
    };
    signal?.addEventListener('abort', cancel, { once: true });
    const settled = (): void => signal?.removeEventListener('abort', cancel);

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
    if (!head.hasBody) settled();

    const stream = head.hasBody
      ? new ReadableStream<Uint8Array>({
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
        })
      : null;
    const response = new Response(stream, { status: head.status, statusText: head.statusText, headers: head.headers });
    Object.defineProperty(response, 'url', { value: head.url });
    return response;
  };
}
