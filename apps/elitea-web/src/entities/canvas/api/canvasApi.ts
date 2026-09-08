/**
 * entities/canvas/api/canvasApi.ts — TanStack Query port of the canvas-scoped
 * slice of `apps/elitea-ui/src/[fsd]/features/chat/api/chat.api.js`'s RTK
 * Query endpoints: `createCanvas`, `editCanvas`, `canvasDetails`,
 * `setAttachmentStorage`, `uploadAttachments`, `removeAttachments`.
 * Query-key based caching + explicit `invalidateQueries` calls replace
 * `providesTags`/`invalidatesTags` — matching the established pattern (e.g.
 * `features/toolkits/indexes/api/indexesApi.ts`).
 *
 * `setAttachmentStorage`/`uploadAttachments`/`removeAttachments` operate on a
 * CONVERSATION id, not a canvas uuid — they live here only because this
 * cluster's brief assigns the whole listed slice of `chat.api.js` to
 * `entities/canvas` (grouped with the canvas CRUD ops in the old file, not by
 * domain). No behavioral significance to the grouping beyond that.
 *
 * `createCanvas`'s old RTK `transformResponse` additionally injected an empty
 * `conversations: []` array onto the response (chat.api.js:295-301). That is
 * local UI/store bookkeeping for whichever feature renders a canvas's loaded-
 * conversations list, not a property of the `Canvas` entity itself
 * (`entities/canvas/model/types.ts` has no such field) — deliberately NOT
 * reproduced here; the owning feature should initialise its own empty list
 * if it needs one.
 *
 * ── GAP DISCLOSURE: uploadAttachments cannot go through eliteaFetch ──
 * `shared/api/http.ts`'s `serializeBody` (http.ts:154-166) JSON-stringifies
 * any non-string request body. `JSON.stringify(new FormData())` silently
 * produces `"{}"` (no throw) — so a `FormData` body sent through
 * `eliteaFetch` would have its real multipart payload silently replaced with
 * an empty JSON object and `Content-Type: application/json`. This is a
 * PRE-EXISTING infra gap, not introduced by this cluster: orval's generated
 * `createArtifact` (`shared/api/generated/artifacts/artifacts.ts`) and an
 * `applications.ts` mutation both already build a `FormData` and pass it
 * through `eliteaFetch` today, so they carry the identical latent bug.
 * Fixing `shared/api/http.ts` is out of scope for an `entities/canvas` unit
 * (F4, foundational, shared by every generated endpoint — not named in this
 * unit's brief).
 *
 * `shared/api/upload.ts` (unit S6) already built a sanctioned XHR sender —
 * `uploadSmallFile` — for the EXACT same URL this endpoint targets
 * (`POST .../attachments/prompt_lib/{projectId}/{conversationId}`, same
 * `overwrite_attachments=1` field). Reusing it here avoids opening a THIRD
 * sanctioned fetch/XHR call site (R-A1/R-A4 fence the existing two:
 * `shared/api/http.ts` and `shared/api/upload.ts`/`artifacts.ts`) and avoids
 * re-deriving upload machinery this unit was told not to re-port. The one
 * real behavioral deviation this causes: the old RTK mutation sent every
 * attachment in ONE multipart POST (multiple `file` fields); the mutation
 * below sends them as N SEQUENTIAL `uploadSmallFile` calls, one per file.
 * Both hit the same endpoint with the same fields per file — only the
 * atomicity and number of round-trips differ. `removeAttachments` carries no
 * FormData (query-string DELETE) and goes through `eliteaFetch` normally.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult, UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { uploadSmallFile } from '@/shared/api/upload';
import type { UploadResult } from '@/shared/api/upload';

import { normaliseCanvas } from '../lib/normalise';
import type { CanvasWire } from '../lib/normalise';
import type { Canvas } from '../model/types';

/** `fetchData<T>` resolves eliteaFetch's enveloped `{data: T, status, headers}` shape — same one-place unwrap as indexesApi.ts. */
async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

const JSON_HEADERS = { 'Content-Type': 'application/json' } as const;
const CANVAS_QUERY_ROOT = ['canvas'] as const;

/* ── canvasDetails — GET elitea_core/canvas/prompt_lib/{projectId}/{id} ── */

export interface CanvasDetailsParams {
  readonly projectId: string | number | undefined;
  readonly id: string | undefined;
}

async function getCanvasDetails(params: CanvasDetailsParams, signal?: AbortSignal): Promise<Canvas> {
  const { projectId, id } = params;
  const wire = await fetchData<CanvasWire>(`/elitea_core/canvas/prompt_lib/${String(projectId)}/${String(id)}`, signal ? { signal } : {});
  return normaliseCanvas(wire);
}

export function useCanvasDetailsQuery(params: CanvasDetailsParams): UseQueryResult<Canvas> {
  const { projectId, id } = params;
  return useQuery({
    queryKey: [...CANVAS_QUERY_ROOT, 'details', projectId, id],
    queryFn: ({ signal }) => getCanvasDetails(params, signal),
    enabled: projectId !== undefined && id !== undefined,
  });
}

/* ── createCanvas — POST elitea_core/canvases/prompt_lib/{projectId} ── */

/**
 * The create's own body, including the RANGE the route splits a stored message
 * on.
 *
 * The four range fields were missing, and their absence was not cosmetic: this
 * endpoint does not take a canvas's CONTENT, it takes a message item and two
 * offsets into it, deletes that item and writes text / canvas / text in its
 * place. Without them the only reachable call was one the server refuses, so
 * the sole working caller of this route in the whole repository was a
 * journey's own HTTP client.
 *
 * `canvas_content_starts_at` / `canvas_content_ends_at` are BYTE offsets — the
 * server slices a Go string with them. `features/chat-messages`'
 * `canvasByteRange` is what measures them; a character index sent here carves
 * a different range than the reader selected and is accepted.
 */
export interface CreateCanvasParams {
  readonly projectId: string | number;
  readonly name?: string;
  readonly canvas_type?: string;
  readonly code_language?: string;
  readonly canvas_content?: string;
  readonly message_group_uuid?: string;
  /** The message group the split happens in — the row id, not the uuid. */
  readonly message_group_id?: number;
  /** The `text_message` item that is split. */
  readonly message_item_id?: number;
  readonly canvas_content_starts_at?: number;
  readonly canvas_content_ends_at?: number;
}

async function createCanvas(params: CreateCanvasParams): Promise<Canvas> {
  const { projectId, ...body } = params;
  const wire = await fetchData<CanvasWire>(`/elitea_core/canvases/prompt_lib/${String(projectId)}`, {
    method: 'POST',
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });
  return normaliseCanvas(wire);
}

/**
 * No `onSuccess` invalidation here — deliberate, matching the old app's own
 * `chat.api.js` `createCanvas` endpoint, whose `invalidatesTags: []` was a
 * hardcoded no-op (a newly-created canvas's uuid can't already have a
 * `canvasDetails` cache entry to invalidate). `useEditCanvasMutation` below
 * DOES invalidate, matching the old app's `editCanvas`/`canvasDetails` tag
 * pairing, which is the one case where a prior cache entry can exist.
 */
export function useCreateCanvasMutation(): UseMutationResult<Canvas, Error, CreateCanvasParams> {
  return useMutation({ mutationFn: createCanvas });
}

/* ── editCanvas — PUT elitea_core/canvas/prompt_lib/{projectId}/{canvasUUID} ── */

export interface EditCanvasParams {
  readonly projectId: string | number;
  readonly canvasUUID: string;
  readonly name?: string;
  readonly canvas_type?: string;
  readonly code_language?: string;
  readonly canvas_content?: string;
}

async function editCanvas(params: EditCanvasParams): Promise<Canvas> {
  const { projectId, canvasUUID, ...body } = params;
  const wire = await fetchData<CanvasWire>(`/elitea_core/canvas/prompt_lib/${String(projectId)}/${canvasUUID}`, {
    method: 'PUT',
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });
  return normaliseCanvas(wire);
}

export function useEditCanvasMutation(): UseMutationResult<Canvas, Error, EditCanvasParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: editCanvas,
    onSuccess: (canvas, variables) =>
      void queryClient.invalidateQueries({ queryKey: [...CANVAS_QUERY_ROOT, 'details', variables.projectId, canvas.uuid] }),
  });
}

/* ── setAttachmentStorage — PUT elitea_core/attachment_storage/prompt_lib/{projectId}/{conversationId} ── */

export interface SetAttachmentStorageParams {
  readonly projectId: string | number;
  readonly conversationId: string;
  readonly toolkit_id: string | number;
}

async function setAttachmentStorage(params: SetAttachmentStorageParams): Promise<unknown> {
  const { projectId, conversationId, toolkit_id } = params;
  return fetchData<unknown>(`/elitea_core/attachment_storage/prompt_lib/${String(projectId)}/${conversationId}`, {
    method: 'PUT',
    headers: JSON_HEADERS,
    body: JSON.stringify({ toolkit_id }),
  });
}

export function useSetAttachmentStorageMutation(): UseMutationResult<unknown, Error, SetAttachmentStorageParams> {
  return useMutation({ mutationFn: setAttachmentStorage });
}

/* ── uploadAttachments — POST elitea_core/attachments/prompt_lib/{projectId}/{conversationId} (multipart) ── */

export interface UploadAttachmentsParams {
  /** `shared/config`'s resolved `Config['vite_server_url']` — caller-supplied, matching every other caller-supplied-baseUrl transport in this codebase (e.g. `shared/api/upload.ts`'s own params, `ExportApplicationButton.tsx`). */
  readonly baseUrl: string;
  readonly projectId: string;
  readonly conversationId: string;
  readonly attachments: readonly File[];
  readonly devToken?: string;
}

/** See module doc's GAP DISCLOSURE: sequential `uploadSmallFile` calls, one per attachment, rather than one atomic multipart POST. */
async function uploadAttachments(params: UploadAttachmentsParams): Promise<readonly UploadResult<unknown>[]> {
  const results: UploadResult<unknown>[] = [];
  for (const file of params.attachments) {
    // eslint-disable-next-line no-await-in-loop -- sequential by design (see module doc's GAP DISCLOSURE): each POST must complete before the next starts, there is no batched endpoint to fan these out to.
    const outcome = await uploadSmallFile({
      baseUrl: params.baseUrl,
      projectId: params.projectId,
      conversationId: params.conversationId,
      file,
      ...(params.devToken !== undefined ? { devToken: params.devToken } : {}),
    });
    results.push(outcome);
  }
  return results;
}

export function useUploadAttachmentsMutation(): UseMutationResult<readonly UploadResult<unknown>[], Error, UploadAttachmentsParams> {
  return useMutation({ mutationFn: uploadAttachments });
}

/* ── removeAttachments — DELETE elitea_core/attachments/prompt_lib/{projectId}/{conversationId} ── */

export interface RemoveAttachmentsParams {
  readonly projectId: string | number;
  readonly conversationId: string;
  /** Filenames to remove — chat.api.js:497-503 accepts either an array of `{name}` objects or a single filename string. */
  readonly attachments: readonly { readonly name: string }[] | string;
  readonly keep_in_storage?: boolean;
}

function buildRemoveAttachmentsQuery(params: RemoveAttachmentsParams): string {
  const query = new URLSearchParams();
  const filenames =
    typeof params.attachments === 'string' ? [params.attachments] : params.attachments.map((attachment) => attachment.name);
  for (const filename of filenames) query.append('filename', filename);
  query.append('keep_in_storage', params.keep_in_storage === true ? '1' : '0');
  return query.toString();
}

async function removeAttachments(params: RemoveAttachmentsParams): Promise<unknown> {
  const { projectId, conversationId } = params;
  const query = buildRemoveAttachmentsQuery(params);
  return fetchData<unknown>(`/elitea_core/attachments/prompt_lib/${String(projectId)}/${conversationId}?${query}`, {
    method: 'DELETE',
  });
}

export function useRemoveAttachmentsMutation(): UseMutationResult<unknown, Error, RemoveAttachmentsParams> {
  return useMutation({ mutationFn: removeAttachments });
}
