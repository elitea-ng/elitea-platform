/**
 * Request-body serialization for the HTTP core (split out of `http.ts` to keep
 * that file inside the 400-line budget; behaviour unchanged).
 */

/** True for the `BodyInit` variants `fetch` already knows how to send verbatim, with their own Content-Type — JSON-stringifying any of these silently discards the payload (`JSON.stringify(new FormData())` is `"{}"`, no throw). */
export function isPreEncodedBody(body: unknown): body is FormData | Blob | URLSearchParams | ArrayBuffer {
  return body instanceof FormData || body instanceof Blob || body instanceof URLSearchParams || body instanceof ArrayBuffer;
}

/**
 * BUG FIX, found while porting Wave-2 unit C1 (chat model/store): this used
 * to `JSON.stringify` every non-string body unconditionally, including
 * `FormData` — `JSON.stringify(new FormData())` returns `"{}"` (FormData has
 * no enumerable own properties), so a multipart upload's real payload was
 * silently replaced with an empty JSON object and no error was ever thrown.
 * Reproduced live: `shared/api/generated/artifacts/artifacts.ts`'s
 * `createArtifact` and `shared/api/generated/applications/applications.ts`'s
 * `uploadApplicationIcon` both build a `FormData` and pass it straight into
 * `eliteaFetch({ body: formData })` — both were silently sending an empty
 * body to the server. `FormData`/`Blob`/`URLSearchParams`/`ArrayBuffer` are
 * passed through unchanged now; `prepare()` below also stops forcing
 * `Content-Type: application/json` onto them, so `fetch` sets its own
 * (for `FormData`, `multipart/form-data` with the correct boundary).
 */
export function serializeBody(method: string, body: unknown, url: string): string | FormData | Blob | URLSearchParams | ArrayBuffer | undefined {
  if (body === undefined) return undefined;
  if (method === 'GET' || method === 'HEAD') {
    throw new TypeError(`http: ${method} ${url} cannot carry a request body`);
  }
  if (typeof body === 'string') return body;
  if (isPreEncodedBody(body)) return body;
  try {
    return JSON.stringify(body);
  } catch (cause) {
    // Programmer error — rethrown with context (§3.6).
    throw new TypeError(`http: request body for ${method} ${url} is not JSON-serializable`, { cause });
  }
}
