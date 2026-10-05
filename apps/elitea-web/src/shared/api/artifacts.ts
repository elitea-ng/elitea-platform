/**
 * shared/api/artifacts.ts — the artifact transports that cannot go through
 * `createHttpClient` (F4's http.ts) because they need a raw `Blob`
 * request/response body rather than JSON. See the `.oxlintrc.json` override
 * comment on this path for why `fetch` is sanctioned here (R-A1).
 *
 * ROUTING (re-verified against services/elitea-main/internal/api/router.go
 * and exercised against the running E2E stack, 2026-08-08 — issue #138):
 * every artifact route lives under `/api/v2/artifacts`, registered by
 * `mountArtifactRoutes` (router.go:255-311). The base URL this module is
 * handed (`config.vite_server_url`, e.g. `/api/v2`) is therefore used AS-IS,
 * with only trailing slashes normalised away.
 *
 *   bucket list  GET  /api/v2/artifacts/buckets/{projectID}
 *   object list  GET  /api/v2/artifacts/objects/{projectID}/{bucket}
 *   download     GET  /api/v2/artifacts/objects/{projectID}/{bucket}/{key...}
 *   upload       POST /api/v2/artifacts/objects/{projectID}/{bucket}
 *
 * This module previously targeted the LEGACY Pylon plugin's URLs
 * (`/artifacts/s3/...` at root level, `/artifacts/artifact/default/...`),
 * citing a router.go:165-169 mount that no longer exists. Those routes 404
 * on elitea-main, which is why no bucket, file table, preview, download or
 * ZIP was reachable in the UI at all (issue #138).
 */
import { resolveCredentialsMode } from './http';
import type { HttpFailure, HttpResult } from './http';

/* ── base URL ─────────────────────────────────────────────────────────────── */

/**
 * `config.vite_server_url` (already `/api/v2`-suffixed) with trailing slashes
 * removed, so the path segments below concatenate cleanly. Nothing is
 * stripped: unlike the legacy S3-proxy routes, the artifact API is mounted
 * entirely UNDER `/api/v2`.
 */
function artifactsBase(baseUrl: string): string {
  return `${baseUrl.replace(/\/+$/, '')}/artifacts`;
}

/* ── shared plumbing ──────────────────────────────────────────────────────── */

/** Exact port of the per-segment encoding in slices/upload.js:100-103. */
function encodeKeySegments(key: string): string {
  return key
    .split('/')
    .map((segment) => encodeURIComponent(segment))
    .join('/');
}

/**
 * Dev bearer + no-cache, gated by import.meta.env.DEV — parity with
 * http.ts's behaviour 6 (statically eliminated from production bundles).
 */
function devHeaders(devToken?: string): Headers {
  const headers = new Headers();
  if (import.meta.env.DEV) {
    if (devToken !== undefined && devToken !== '') headers.set('Authorization', `Bearer ${devToken}`);
    headers.set('Cache-Control', 'no-cache');
  }
  return headers;
}

/**
 * Old app's credentials handling across these near-identical fetches was
 * inconsistent: slices/upload.js:117's axios PUT sets `withCredentials:true`
 * unconditionally; utils.jsx:390's fetchArtifactBlobUrl and utils.jsx:508's
 * downloadArtifactsAsZip set no credentials option at all (default
 * 'same-origin'); useArtifactContentFetch.hooks.js:84 sets 'include'
 * explicitly. Applying F4's dynamic cross-origin rule (§5.4 behaviour 1)
 * uniformly here both preserves 'include' at the site that had it and closes
 * the silent-cookie-drop gap at the sites that didn't — the same rationale
 * http.ts itself is built on.
 */
function credentialsFor(resolvedBaseUrl: string): RequestCredentials {
  return resolveCredentialsMode(resolvedBaseUrl, window.location.origin);
}

function isAbortError(cause: unknown): boolean {
  return typeof cause === 'object' && cause !== null && (cause as { name?: unknown }).name === 'AbortError';
}

function networkFailure(cause: unknown, url: string): HttpFailure {
  if (isAbortError(cause)) return { kind: 'aborted', url };
  const message = cause instanceof Error ? cause.message : String(cause);
  return { kind: 'network', url, message: `artifacts: request to ${url} failed: ${message}`, cause };
}

/** Body-parsing twin of http.ts's toResult, for JSON-returning endpoints. */
async function parseJsonBody(response: Response): Promise<unknown> {
  const text = await response.text();
  if (text === '') return undefined;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    // Handled (§3.6): a non-JSON body is surfaced as raw text, not a crash.
    return text;
  }
}

async function toJsonResult(response: Response): Promise<HttpResult<unknown>> {
  const data = await parseJsonBody(response);
  if (!response.ok) {
    return { ok: false, error: { kind: 'http', status: response.status, url: response.url, body: data } };
  }
  return { ok: true, status: response.status, data, headers: response.headers };
}

async function toBlobResult(response: Response): Promise<HttpResult<Blob>> {
  if (!response.ok) {
    const body = await parseJsonBody(response);
    return { ok: false, error: { kind: 'http', status: response.status, url: response.url, body } };
  }
  return { ok: true, status: response.status, data: await response.blob(), headers: response.headers };
}

/* ── upload ───────────────────────────────────────────────────────────────── */

export interface UploadArtifactObjectParams {
  /** config.vite_server_url, e.g. '/api/v2' — used as-is. */
  readonly baseUrl: string;
  readonly projectId: string;
  readonly bucket: string;
  /** Unencoded; may contain '/' for a folder path — sent as the part's filename. */
  readonly fileKey: string;
  readonly file: Blob;
  readonly contentType?: string;
  readonly devToken?: string;
  readonly signal?: AbortSignal;
}

/**
 * `POST /api/v2/artifacts/objects/{projectID}/{bucket}` (router.go:288).
 *
 * `overwrite=true` (objects.go:301) is unconditional because the caller has
 * ALREADY resolved duplicates before reaching here: `useArtifactUpload`'s
 * duplicate dialog either renames the file (`keepBothFileNames`) or the user
 * chose to replace. Without it the server 409s on the replace branch, which
 * the UI has no way to interpret as anything but a failure. This also
 * preserves the legacy S3 PUT's overwrite semantics.
 */
export function buildObjectUploadUrl(baseUrl: string, projectId: string, bucket: string): string {
  return `${artifactsBase(baseUrl)}/objects/${encodeURIComponent(projectId)}/${encodeURIComponent(bucket)}?overwrite=true`;
}

/**
 * Uploads one object as `multipart/form-data`. The object's KEY is the
 * part's filename, not a path segment: `UploadObject` parses
 * `Content-Disposition` directly rather than calling `part.FileName()`,
 * precisely so a multi-segment key like `folder/a.txt` survives instead of
 * being truncated to `a.txt` (objects.go:326-344).
 *
 * NOTE (parity gap, unchanged from the legacy transport): `fetch` cannot
 * report upload progress the way the old app's `axios` `onUploadProgress`
 * did. §2.4 sanctions XMLHttpRequest for upload progress only in
 * `shared/api/upload.ts` (the chunked-attachment path); the bucket-upload
 * path was not in that carve-out.
 */
export async function uploadArtifactObject(params: UploadArtifactObjectParams): Promise<HttpResult<void>> {
  const url = buildObjectUploadUrl(params.baseUrl, params.projectId, params.bucket);
  const headers = devHeaders(params.devToken);
  // Deliberately NO Content-Type header: fetch must set the multipart
  // boundary itself, and Go's MultipartReader rejects a body whose declared
  // boundary does not match.
  const contentType = params.contentType !== undefined && params.contentType !== ''
    ? params.contentType
    : params.file.type;
  // `Blob.slice` with a type is a zero-copy retype — building a new Blob from
  // the file's bytes would duplicate the whole payload in memory.
  const part = contentType === params.file.type ? params.file : params.file.slice(0, params.file.size, contentType);
  const body = new FormData();
  // Third argument, not `new File(...)`: the File constructor replaces '/'
  // with ':' in its name, which would mangle every folder-scoped key.
  body.append('file', part, params.fileKey);
  const credentials = credentialsFor(params.baseUrl);

  let response: Response;
  try {
    response = await fetch(url, { method: 'POST', headers, body, credentials, signal: params.signal ?? null });
  } catch (cause) {
    return { ok: false, error: networkFailure(cause, url) };
  }
  if (!response.ok) {
    const errorBody = await parseJsonBody(response);
    return { ok: false, error: { kind: 'http', status: response.status, url: response.url, body: errorBody } };
  }
  // Read the answer to the end even though nothing here uses it. The API
  // answers `Cache-Control: no-store`, so the HTTP cache no longer drains an
  // unread body: left unread, the request never finishes loading (Chromium
  // never reports it complete, and the connection is held until GC).
  await response.arrayBuffer().catch(() => undefined);
  return { ok: true, status: response.status, data: undefined, headers: response.headers };
}

/* ── bucket / object list ─────────────────────────────────────────────────── */

export interface ListBucketsParams {
  readonly baseUrl: string;
  readonly projectId: string;
  readonly signal?: AbortSignal;
}

/** `GET /api/v2/artifacts/buckets/{projectID}` (router.go:279). */
export function buildBucketListUrl(baseUrl: string, projectId: string): string {
  return `${artifactsBase(baseUrl)}/buckets/${encodeURIComponent(projectId)}`;
}

export async function listBuckets(params: ListBucketsParams): Promise<HttpResult<unknown>> {
  const url = buildBucketListUrl(params.baseUrl, params.projectId);
  const credentials = credentialsFor(params.baseUrl);
  let response: Response;
  try {
    response = await fetch(url, { credentials, signal: params.signal ?? null });
  } catch (cause) {
    return { ok: false, error: networkFailure(cause, url) };
  }
  return toJsonResult(response);
}

export interface ListArtifactsParams {
  readonly baseUrl: string;
  readonly projectId: string;
  readonly bucket: string;
  readonly signal?: AbortSignal;
}

/**
 * `GET /api/v2/artifacts/objects/{projectID}/{bucket}` (router.go:285).
 *
 * The response is FLAT — one page of every key under the bucket, with no
 * `delimiter`, so nested keys arrive whole (`folder/a.txt`) rather than as
 * `common_prefixes`. The artifacts table does its own folder grouping from
 * the key, which is why asking the server to collapse prefixes would hide
 * the very rows it needs.
 */
export function buildArtifactListUrl(baseUrl: string, projectId: string, bucket: string): string {
  return `${artifactsBase(baseUrl)}/objects/${encodeURIComponent(projectId)}/${encodeURIComponent(bucket)}`;
}

export async function listArtifacts(params: ListArtifactsParams): Promise<HttpResult<unknown>> {
  const url = buildArtifactListUrl(params.baseUrl, params.projectId, params.bucket);
  const credentials = credentialsFor(params.baseUrl);
  let response: Response;
  try {
    response = await fetch(url, { credentials, signal: params.signal ?? null });
  } catch (cause) {
    return { ok: false, error: networkFailure(cause, url) };
  }
  return toJsonResult(response);
}

/* ── object download + ZIP multi-download ─────────────────────────────────── */

export interface FetchArtifactBlobParams {
  /** config.vite_server_url, used AS-IS. */
  readonly baseUrl: string;
  readonly projectId: string;
  readonly bucket: string;
  /** May contain '/' for a nested path; each segment is encoded separately. */
  readonly filePath: string;
  // `| undefined` explicit so downloadArtifactsAsZip can forward its own
  // (already-optional) fields straight through under exactOptionalPropertyTypes.
  readonly devToken?: string | undefined;
  readonly signal?: AbortSignal | undefined;
}

/**
 * `GET /api/v2/artifacts/objects/{projectID}/{bucket}/{key...}`
 * (router.go:288). The key is a trailing chi wildcard, so its '/' separators
 * must stay literal while each SEGMENT is percent-encoded —
 * `encodeURIComponent` over the whole key would turn `a/b.txt` into
 * `a%2Fb.txt` and miss the route. Verified against the running stack: a key
 * with spaces resolves when its segments are encoded (chi decodes them) and
 * fails when they are not.
 */
export function buildArtifactContentUrl(baseUrl: string, projectId: string, bucket: string, filePath: string): string {
  return `${artifactsBase(baseUrl)}/objects/${encodeURIComponent(projectId)}/${encodeURIComponent(bucket)}/${encodeKeySegments(filePath)}`;
}

/**
 * Unifies the preview, download and ZIP-member reads — all three want the
 * object's raw bytes. `credentials` is resolved dynamically (see
 * `credentialsFor` above).
 */
export async function fetchArtifactBlob(params: FetchArtifactBlobParams): Promise<HttpResult<Blob>> {
  const url = buildArtifactContentUrl(params.baseUrl, params.projectId, params.bucket, params.filePath);
  const headers = devHeaders(params.devToken);
  const credentials = credentialsFor(params.baseUrl);
  let response: Response;
  try {
    response = await fetch(url, { headers, credentials, signal: params.signal ?? null });
  } catch (cause) {
    return { ok: false, error: networkFailure(cause, url) };
  }
  return toBlobResult(response);
}

/**
 * Structural interface for a ZIP archiver — deliberately NOT a dependency on
 * the `jszip` package (old: utils.jsx:497 `const JSZip = (await
 * import('jszip')).default`). `jszip` is not in this app's dependency set
 * yet (package.json is unit F1's exclusive file; S6 must not touch it) — the
 * caller (the Wave-2 feature that wires the real download button) injects a
 * `JSZip` instance, which already satisfies this shape, or any equivalent.
 */
export interface ZipArchiver {
  file(path: string, data: Blob): void;
  generateAsync(options: { type: 'blob' }): Promise<Blob>;
}

export interface DownloadArtifactsAsZipParams {
  readonly baseUrl: string;
  readonly projectId: string;
  readonly bucket: string;
  /** Already-expanded flat file list (old app's folder→file expansion against `bucketContents`, utils.jsx:455-471, is a selection/UI concern left to the caller). */
  readonly filenames: readonly string[];
  readonly archiver: ZipArchiver;
  /** Stripped from each entry's path inside the archive (utils.jsx:517-523). */
  readonly currentPrefix?: string;
  readonly devToken?: string;
  readonly signal?: AbortSignal;
  readonly onProgress?: (current: number, total: number, filename: string) => void;
}

export type DownloadArtifactsAsZipResult =
  | { readonly status: 'ok'; readonly blob: Blob }
  | { readonly status: 'cancelled' }
  | { readonly status: 'error'; readonly error: HttpFailure; readonly filename: string };

function stripCurrentPrefix(filename: string, currentPrefix?: string): string {
  if (currentPrefix === undefined || currentPrefix === '') return filename;
  const normalized = currentPrefix.endsWith('/') ? currentPrefix : `${currentPrefix}/`;
  return filename.startsWith(normalized) ? filename.slice(normalized.length) : filename;
}

/**
 * Sequential per-file fetch + ZIP assembly (old: utils.jsx:444-543
 * `downloadArtifactsAsZip`). Stops and reports `cancelled` on an empty file
 * list or an aborted signal (parity: utils.jsx:487-491's `onCancel` path);
 * stops and reports the failing file on the first HTTP/network error
 * (parity: the old code's per-file `throw`, which the caller's `.catch`
 * surfaced — here it is a Result, never a throw, per §3.6).
 */
export async function downloadArtifactsAsZip(params: DownloadArtifactsAsZipParams): Promise<DownloadArtifactsAsZipResult> {
  const { filenames } = params;
  if (filenames.length === 0) return { status: 'cancelled' };

  for (let index = 0; index < filenames.length; index++) {
    if (params.signal?.aborted === true) return { status: 'cancelled' };
    const filename = filenames[index];
    if (filename === undefined) continue;

    const result = await fetchArtifactBlob({
      baseUrl: params.baseUrl,
      projectId: params.projectId,
      bucket: params.bucket,
      filePath: filename,
      devToken: params.devToken,
      signal: params.signal,
    });
    if (!result.ok) {
      if (result.error.kind === 'aborted') return { status: 'cancelled' };
      return { status: 'error', error: result.error, filename };
    }

    params.archiver.file(stripCurrentPrefix(filename, params.currentPrefix), result.data);
    params.onProgress?.(index + 1, filenames.length, filename);
  }

  const blob = await params.archiver.generateAsync({ type: 'blob' });
  return { status: 'ok', blob };
}
