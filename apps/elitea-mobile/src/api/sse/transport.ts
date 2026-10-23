import { fetch as expoFetch } from 'expo/fetch';

/**
 * One of two importers of `expo/fetch` (enforced by ESLint; the other is
 * src/api/noRedirectFetch.ts, for the token endpoints).
 *
 * REST goes through the global fetch (openapi-fetch builds a `Request` that
 * `expo/fetch` cannot read, plan F11). Event streams need an incrementally
 * readable body, which only `expo/fetch` provides on native (F10), so this
 * module adapts it to the small `StreamTransport` shape the resumable stream
 * consumes. Tests inject a fake transport instead (jest-expo stubs the native
 * fetch module).
 */

export interface StreamResponse {
  status: number;
  headers: { get(name: string): string | null };
  /** The final URL, when the platform reports it. */
  url?: string;
  /** The body as byte chunks; null when there is none. */
  body: AsyncIterable<Uint8Array> | null;
}

export interface StreamRequestInit {
  headers: Record<string, string>;
  signal: AbortSignal;
}

export type StreamTransport = (url: string, init: StreamRequestInit) => Promise<StreamResponse>;

async function* readChunks(stream: ReadableStream<Uint8Array>): AsyncGenerator<Uint8Array> {
  const reader = stream.getReader();
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) return;
      if (value && value.byteLength > 0) yield value;
    }
  } finally {
    // Releases the native stream when the consumer stops early (terminal
    // frame, abort, watchdog). Errors here are irrelevant: we are leaving.
    reader.cancel().catch(() => {});
  }
}

export const expoStreamTransport: StreamTransport = async (url, init) => {
  const response = await expoFetch(url, {
    method: 'GET',
    headers: init.headers,
    signal: init.signal,
    // A redirect must never carry the bearer token anywhere: refuse it.
    redirect: 'error',
  });
  return {
    status: response.status,
    headers: response.headers,
    url: response.url,
    body: response.body ? readChunks(response.body) : null,
  };
};
