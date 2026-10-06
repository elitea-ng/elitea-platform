import type { HttpResult } from './http';

/** Bound retained artifact bytes. Never read private error bodies. */
export async function boundedArtifactBlob(response: Response, maximumBytes: number): Promise<HttpResult<Blob>> {
  if (!response.ok) {
    void response.body?.cancel().catch(() => undefined);
    return { ok: false, error: { kind: 'http', status: response.status, url: response.url, body: undefined } };
  }
  if (!Number.isSafeInteger(maximumBytes) || maximumBytes < 1 || !response.body) {
    void response.body?.cancel().catch(() => undefined);
    return readFailure(response.url);
  }
  const reader = response.body.getReader();
  const bytes = new Uint8Array(maximumBytes);
  let length = 0;
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      if (next.value.byteLength > maximumBytes - length) {
        void reader.cancel().catch(() => undefined);
        return readFailure(response.url);
      }
      // Use one bounded buffer, even when the stream has many small chunks.
      bytes.set(next.value, length);
      length += next.value.byteLength;
    }
  } finally {
    reader.releaseLock();
  }
  return {
    ok: true, status: response.status, headers: response.headers,
    data: new Blob([bytes.subarray(0, length)], { type: response.headers.get('Content-Type') ?? '' }),
  };
}

function readFailure(url: string): HttpResult<Blob> {
  return { ok: false, error: { kind: 'network', url, message: 'Artifact response cannot be read within its byte limit.', cause: undefined } };
}
