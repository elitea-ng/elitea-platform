/**
 * shared/lib/download.ts — spec §5.7 row 4 (unit S6): markdown export →
 * blob → anchor-click download.
 * Old location: apps/elitea-ui/src/pages/Common/Components/useExport.js:78-101.
 *
 * `triggerBlobDownload` deliberately has NO `beforeunload` guard. The old
 * app's own comment explains why this shape exists at all: "Use programmatic
 * download to avoid triggering beforeunload event (issue 3184)"
 * (useExport.js:80) — a direct `window.location.href = url` navigation could
 * fire a SPA's `beforeunload` prompt; the blob+anchor pattern never
 * navigates the page, so nothing triggers it. The blob+anchor pattern IS the
 * fix. Do not "fix" this file by adding a beforeunload listener — that would
 * reintroduce the exact defect this module exists to avoid.
 *
 * R-A1 note: `fetch` is called here as a SANCTIONED deviation (see the
 * `.oxlintrc.json` override comment on this path) because the export flow
 * needs `response.blob()` and the `Content-Disposition` header, neither of
 * which `shared/api/http.ts`'s `HttpResult<T>` can surface (F4 owns that
 * file; it was not edited for this).
 */
import { resolveCredentialsMode } from '../api/http';
import { sendRaw } from '../api/httpNative';

/* ── row 4a: the reusable blob → anchor-click primitive ──────────────────── */

/**
 * Creates an object URL for `blob`, clicks a detached anchor to trigger the
 * browser's save-as flow, then revokes the URL. No `beforeunload` guard —
 * see the module doc above.
 */
export function triggerBlobDownload(blob: Blob, filename: string): void {
  const blobUrl = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = blobUrl;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  document.body.removeChild(anchor);
  URL.revokeObjectURL(blobUrl);
}

/**
 * Exact port of shared/lib/helpers/download.helpers.js's
 * `getFilenameFromContentDisposition` (old app,
 * [fsd]/shared/lib/helpers/download.helpers.js:10-21): prefers the RFC 5987
 * `filename*=UTF-8''...` parameter, falls back to the legacy ASCII
 * `filename="..."`, and to `fallback` when neither is present.
 */
export function filenameFromContentDisposition(contentDisposition: string | null, fallback: string): string {
  if (contentDisposition === null || contentDisposition === '') return fallback;
  const extended = /filename\*=(?:UTF-8'')?([^;\n]+)/i.exec(contentDisposition);
  const extendedValue = extended?.[1];
  if (extendedValue !== undefined) {
    try {
      return decodeURIComponent(extendedValue.trim());
    } catch {
      // Handled (§3.6): a malformed percent-escape is not a crash.
      return extendedValue.trim();
    }
  }
  const plain = /filename="?([^";\n]+)"?/.exec(contentDisposition);
  return plain?.[1] ?? fallback;
}

/* ── row 4b: the fetch → blob → download orchestrator ────────────────────── */

export interface ExportMarkdownParams {
  /** config.vite_server_url, USED AS-IS — this endpoint is under /api/v2, no strip (unlike artifacts.ts rows 1/5). */
  readonly baseUrl: string;
  readonly projectId: string;
  readonly applicationId: string;
  /** old app: only sent when set (useExport.js:69-73/74-75) — public-app published versions, or the single selected version. */
  readonly followVersionIds?: readonly string[];
  /** Attached as `Authorization: Bearer …` ONLY under import.meta.env.DEV (parity with http.ts behaviour 6). */
  readonly devToken?: string;
  readonly signal?: AbortSignal;
}

type ExportFailure =
  | { readonly kind: 'http'; readonly status: number }
  | { readonly kind: 'network'; readonly message: string }
  | { readonly kind: 'aborted' };

export type ExportMarkdownResult =
  | { readonly ok: true; readonly filename: string }
  | { readonly ok: false; readonly error: ExportFailure };

/** Exact port of useExport.js:70-76's query-string assembly. */
export function buildMarkdownExportUrl(baseUrl: string, projectId: string, applicationId: string, followVersionIds?: readonly string[]): string {
  const query = new URLSearchParams({ format: 'md' });
  if (followVersionIds !== undefined && followVersionIds.length > 0) {
    query.set('follow_version_ids', followVersionIds.join(','));
  }
  const base = baseUrl.endsWith('/') ? baseUrl.slice(0, -1) : baseUrl;
  return `${base}/elitea_core/export_import/prompt_lib/${encodeURIComponent(projectId)}/${encodeURIComponent(applicationId)}?${query.toString()}`;
}

/** A fetch that never answered: aborted, or a network error. Shared by both orchestrators below. */
function toExportFailure(cause: unknown): Extract<ExportFailure, { kind: 'network' | 'aborted' }> {
  if (typeof cause === 'object' && cause !== null && (cause as { name?: unknown }).name === 'AbortError') {
    return { kind: 'aborted' };
  }
  const message = cause instanceof Error ? cause.message : String(cause);
  return { kind: 'network', message: `download: export request failed: ${message}` };
}

/**
 * Fetch → blob → anchor-click download in one call (old: useExport.js:78-101).
 * `fallbackName` becomes `${fallbackName}.md` when the server sends no
 * Content-Disposition header (or one without a filename parameter).
 */
export async function exportMarkdown(params: ExportMarkdownParams, fallbackName: string): Promise<ExportMarkdownResult> {
  const url = buildMarkdownExportUrl(params.baseUrl, params.projectId, params.applicationId, params.followVersionIds);
  const headers = new Headers();
  if (import.meta.env.DEV && params.devToken !== undefined && params.devToken !== '') {
    headers.set('Authorization', `Bearer ${params.devToken}`);
  }
  const credentials = resolveCredentialsMode(params.baseUrl, window.location.origin);

  let response: Response;
  try {
    response = await sendRaw(fetch, url, { headers, credentials, signal: params.signal ?? null });
  } catch (cause) {
    return { ok: false, error: toExportFailure(cause) };
  }
  if (!response.ok) {
    return { ok: false, error: { kind: 'http', status: response.status } };
  }
  const blob = await response.blob();
  const filename = filenameFromContentDisposition(response.headers.get('Content-Disposition'), `${fallbackName}.md`);
  triggerBlobDownload(blob, filename);
  return { ok: true, filename };
}

/* ── row 4c: a generic API GET → blob → download (ADR-0024 WP9) ─────────── */

export interface ApiDownloadParams {
  /** The API base the caller's client is configured with, e.g. `/api/v2`. */
  readonly baseUrl: string;
  /** Server-relative path under `baseUrl`, the generated `get<Op>Url()` shape. */
  readonly path: string;
  /** Used when the server sends no `Content-Disposition` filename. */
  readonly fallbackName: string;
  readonly signal?: AbortSignal;
}

export type ApiDownloadFailure =
  | { readonly kind: 'http'; readonly status: number; readonly reason: string | undefined }
  | { readonly kind: 'network'; readonly message: string }
  | { readonly kind: 'aborted' };

export type ApiDownloadResult =
  | { readonly ok: true; readonly filename: string }
  | { readonly ok: false; readonly error: ApiDownloadFailure };

/** The `{"error": "…"}` sentence every elitea-main refusal carries, when the body is one. */
async function refusalReason(response: Response): Promise<string | undefined> {
  if (!(response.headers.get('content-type') ?? '').includes('application/json')) return undefined;
  try {
    const body = (await response.json()) as { error?: unknown };
    return typeof body.error === 'string' && body.error !== '' ? body.error : undefined;
  } catch {
    // Handled (§3.6): a body that is not JSON after all is not a crash.
    return undefined;
  }
}

/**
 * GET a binary route through the same fetch/credentials path as
 * `exportMarkdown`, then save it under the server's filename. The typed
 * client reads every body as text (`http.ts`'s `toResult`), so a zip fetched
 * through it arrives corrupted; this file is the sanctioned place for the raw
 * fetch (see the `.oxlintrc.json` override on this path).
 */
export async function downloadFromApi(params: ApiDownloadParams): Promise<ApiDownloadResult> {
  const base = params.baseUrl.endsWith('/') ? params.baseUrl.slice(0, -1) : params.baseUrl;
  const url = `${base}${params.path}`;
  const credentials = resolveCredentialsMode(params.baseUrl, window.location.origin);

  let response: Response;
  try {
    response = await sendRaw(fetch, url, { credentials, signal: params.signal ?? null });
  } catch (cause) {
    return { ok: false, error: toExportFailure(cause) };
  }
  if (!response.ok) {
    return { ok: false, error: { kind: 'http', status: response.status, reason: await refusalReason(response) } };
  }
  const blob = await response.blob();
  const filename = filenameFromContentDisposition(response.headers.get('Content-Disposition'), params.fallbackName);
  triggerBlobDownload(blob, filename);
  return { ok: true, filename };
}
