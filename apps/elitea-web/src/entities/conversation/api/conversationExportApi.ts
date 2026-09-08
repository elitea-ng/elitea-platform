/**
 * Conversation export (issue 851) — the transcript as a file the reader keeps.
 *
 * The rail's row menu has always rendered an "Export" entry. It was disabled,
 * its two sub-items were labelled `Option1`/`Option2`, and there was no route
 * behind either of them. This module is the client half of the route that
 * closes that gap: `GET /elitea_core/conversation_export/prompt_lib/{project}/
 * {conversation}?format=md|json`.
 *
 * WHY THIS IS NOT THE GENERATED CLIENT. `exportConversation` in
 * `shared/api/generated/chat` exists and is a real operation, but it goes
 * through `eliteaFetch`, which reads every body as TEXT and hands back an
 * envelope — no `Blob`, and no way to reach the response for the browser's
 * save-as flow. `shared/lib/download.ts` is the sanctioned place for the raw
 * fetch this needs (its own module doc explains the fetch exemption), and it
 * already carries the `Content-Disposition` filename reader and the
 * blob+anchor download the old app used for exactly this class of control.
 *
 * The URL still comes from the GENERATED builder, so the path and its query
 * parameters stay derived from `v2.yaml` rather than restated here — the one
 * thing a hand-written download would otherwise silently drift on.
 */
import { getConfig } from '@/shared/config';
import { downloadFromApi, type ApiDownloadResult } from '@/shared/lib/download';
import { getExportConversationUrl } from '@/shared/api/generated/chat/chat';

/**
 * The two formats the server serves, which are the two menu entries.
 *
 * NOT exported: the slice's public API is exactly at its export budget, so the
 * one feature that offers the control declares a structurally identical union
 * of its own (`ConversationItem.menu.tsx`) and this stays the parameter type a
 * caller is checked against.
 */
type ConversationExportFormat = 'md' | 'json';

export interface DownloadConversationExportParams {
  readonly projectId: string | number;
  readonly conversationId: string | number;
  /**
   * Only used to name the saved file when the server sends no
   * `Content-Disposition` filename. The server normally does send one, and it
   * sanitises the name there — this is the last resort, not the primary.
   */
  readonly conversationName?: string | undefined;
  readonly format: ConversationExportFormat;
  readonly signal?: AbortSignal | undefined;
}

/**
 * The API base this app's generated client is configured with.
 *
 * `getConfig()` rather than a prop: the download is triggered from a row menu
 * deep inside the rail, and threading the base URL from `app/` to there would
 * add a prop to five components for one string that never changes at runtime.
 * The `'/api/v2'` fallback is the same default `configureGeneratedClient` is
 * given when the config is unavailable in a test render.
 */
function apiBaseUrl(): string {
  const result = getConfig();
  const configured = result.status === 'ok' ? result.config.vite_server_url : undefined;
  return configured !== undefined && configured !== '' ? configured : '/api/v2';
}

/**
 * A last-resort filename, used only when the response carries no
 * `Content-Disposition`. Sanitised the same way the server sanitises its own,
 * because a name with a path separator in it is not a filename.
 */
function fallbackExportFilename(conversationName: string | undefined, conversationId: string | number, format: ConversationExportFormat): string {
  const stem = (conversationName ?? '').replaceAll(/[^A-Za-z0-9\-_]+/gu, '_').replaceAll(/^_+|_+$/gu, '');
  return `${stem === '' ? `conversation_${String(conversationId)}` : stem}.${format}`;
}

/**
 * GET the export route and hand the answer to the browser's save-as flow.
 *
 * Resolves the outcome; never rejects on a refusal — the caller turns a
 * failure into a toast, and a rejected promise inside a menu click handler is
 * an unhandled rejection in every browser console.
 */
export async function downloadConversationExport(params: DownloadConversationExportParams): Promise<ApiDownloadResult> {
  return downloadFromApi({
    baseUrl: apiBaseUrl(),
    path: getExportConversationUrl(String(params.projectId), String(params.conversationId), { format: params.format }),
    fallbackName: fallbackExportFilename(params.conversationName, params.conversationId, params.format),
    ...(params.signal !== undefined ? { signal: params.signal } : {}),
  });
}
