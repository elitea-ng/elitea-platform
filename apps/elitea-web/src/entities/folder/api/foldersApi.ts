/**
 * Hand-written REST layer for the 7 folder-domain endpoints of
 * `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/api/
 * conversationList.api.js:22-137` (unit C2) — folderCreate/foldersList/
 * folderConversations/dateGroupConversations/folderUpdate/deleteFolder/
 * folderPinUpdate. Same handwritten-manifest rationale as
 * `entities/conversation/api/conversationApi.ts`'s module doc: no OpenAPI
 * schema documents any `/elitea_core/folder/...` path, so every route below
 * is a new `source:"handwritten"` manifest entry.
 *
 * ── wire → domain normalisation, disclosed ──
 * `entities/folder/model/types.ts`'s `GroupedFoldersResponse`/`Folder`/
 * `FolderConversationRef` are already-camelCase domain shapes (`dateGroups`,
 * `selectedConversationId`, `totalFolders`, `updatedAt`, `createdAt`), but
 * the real wire payload is snake_case (`date_groups`, `selected_conversation_id`,
 * `total_folders`, `updated_at`, `created_at` — confirmed against
 * `useQueryFoldersList.hooks.js:141-162` and `conversationList.helpers.js:24-25`;
 * `isPlayback` is the one field the old app reads already-camelCase off a
 * conversation, per `conversationList.helpers.js:28-29`, so it is passed
 * through unrenamed). This file therefore does real wire→domain mapping,
 * unlike `conversationApi.ts`'s `ConversationWire` (loosely typed catch-all,
 * no renaming needed there). The wire types + normalisers are kept LOCAL to
 * this file rather than added to `../lib/normalise.ts`: that module's own
 * doc frames its two exports (`conversationMatchId`,
 * `flattenGroupedConversations`) as operating on the ALREADY-normalised
 * `GroupedFoldersResponse` domain shape, not on wire parsing — a different
 * concern this unit's brief didn't ask to extend.
 *
 * A folder's pinned state is wire-nested at `meta.is_pinned`, not a
 * top-level `is_pinned` — confirmed by `useEditFolder.js:54-56`'s own
 * optimistic-update shape (`{...item, meta: {...item.meta, is_pinned}}`) —
 * normalised here onto `Folder.isPinned` (top-level, per the already-landed
 * domain type).
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

import type { Folder, FolderConversationRef, GroupedFoldersResponse } from '../model/types';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

const JSON_HEADERS = { 'Content-Type': 'application/json' } as const;
const FOLDER_QUERY_ROOT = ['folder'] as const;

/* ── wire types + normalisers ── */

interface FolderConversationRefWire {
  readonly id: string;
  readonly name?: string;
  readonly is_private?: boolean;
  /** Integer on the wire (`folders` handler `conversationItem.AuthorID`). */
  readonly author_id?: number | string;
  readonly updated_at?: string;
  readonly created_at?: string;
  readonly isPlayback?: boolean;
  readonly [key: string]: unknown;
}

function normaliseFolderConversationRef(wire: FolderConversationRefWire): FolderConversationRef {
  return {
    id: wire.id,
    ...(wire.name !== undefined ? { name: wire.name } : {}),
    ...(wire.is_private !== undefined ? { isPrivate: wire.is_private } : {}),
    // Keep the value as a string. Every consumer compares it against a
    // string user id. The normaliser dropped this field before. The row
    // menu's author guard then compared `undefined` with `undefined`. Delete
    // and Edit stayed enabled on another member's conversation.
    ...(wire.author_id !== undefined ? { authorId: String(wire.author_id) } : {}),
    ...(wire.updated_at !== undefined ? { updatedAt: wire.updated_at } : {}),
    ...(wire.created_at !== undefined ? { createdAt: wire.created_at } : {}),
    ...(wire.isPlayback !== undefined ? { isPlayback: wire.isPlayback } : {}),
  };
}

interface FolderWire {
  readonly id: string;
  readonly name: string;
  readonly conversations?: readonly FolderConversationRefWire[];
  readonly total?: number;
  readonly offset?: number;
  readonly meta?: { readonly is_pinned?: boolean; readonly [key: string]: unknown };
  readonly [key: string]: unknown;
}

/** Also used by `folderCreate` — the server response never has `conversations`, so `wire.conversations ?? []` naturally yields `[]`, matching the old app's explicit `transformResponse` (`conversationList.api.js:38-44`). */
function normaliseFolder(wire: FolderWire): Folder {
  return {
    id: wire.id,
    name: wire.name,
    conversations: (wire.conversations ?? []).map(normaliseFolderConversationRef),
    ...(wire.total !== undefined ? { total: wire.total } : {}),
    ...(wire.offset !== undefined ? { offset: wire.offset } : {}),
    ...(wire.meta?.is_pinned !== undefined ? { isPinned: wire.meta.is_pinned } : {}),
  };
}

/**
 * The bucket wire shape carries `total`/`offset` exactly as `FolderWire` does
 * — the grouped listing pages every bucket and reports the remainder. Both
 * were absent from this interface, so the normaliser below dropped them and
 * the rail's per-bucket load-more had nothing to fire on.
 */
interface DateGroupWire {
  readonly name: string;
  readonly conversations?: readonly FolderConversationRefWire[];
  readonly total?: number;
  readonly offset?: number;
}

interface GroupedFoldersResponseWire {
  readonly pinned?: { readonly conversations?: readonly FolderConversationRefWire[] };
  readonly date_groups?: readonly DateGroupWire[];
  readonly folders?: readonly FolderWire[];
  readonly selected_conversation_id?: string;
  readonly total_folders?: number;
}

function normaliseGroupedFoldersResponse(wire: GroupedFoldersResponseWire): GroupedFoldersResponse {
  return {
    pinned: { conversations: (wire.pinned?.conversations ?? []).map(normaliseFolderConversationRef) },
    dateGroups: (wire.date_groups ?? []).map((group) => ({
      name: group.name,
      conversations: (group.conversations ?? []).map(normaliseFolderConversationRef),
      ...(group.total !== undefined ? { total: group.total } : {}),
      ...(group.offset !== undefined ? { offset: group.offset } : {}),
    })),
    folders: (wire.folders ?? []).map(normaliseFolder),
    ...(wire.selected_conversation_id !== undefined ? { selectedConversationId: wire.selected_conversation_id } : {}),
    totalFolders: wire.total_folders ?? 0,
  };
}

interface FolderConversationsPageWire {
  readonly conversations?: readonly FolderConversationRefWire[];
  readonly total?: number;
  readonly offset?: number;
  readonly [key: string]: unknown;
}

/** One page of a single folder's or date-group's conversations — `Conversations.jsx:156,166,218,228` reads exactly `result.conversations`/`result.total` off this response. */
export interface FolderConversationsPage {
  readonly conversations: readonly FolderConversationRef[];
  readonly total?: number;
  readonly offset?: number;
}

function normaliseFolderConversationsPage(wire: FolderConversationsPageWire): FolderConversationsPage {
  return {
    conversations: (wire.conversations ?? []).map(normaliseFolderConversationRef),
    ...(wire.total !== undefined ? { total: wire.total } : {}),
    ...(wire.offset !== undefined ? { offset: wire.offset } : {}),
  };
}

/* ── folderCreate — POST elitea_core/folder/prompt_lib/{projectId} ── */
/* manifest: folder.create */

export interface FolderCreateParams {
  readonly projectId: string | number;
  readonly name: string;
  readonly [key: string]: unknown;
}

export async function folderCreate(params: FolderCreateParams): Promise<Folder> {
  const { projectId, ...body } = params;
  const wire = await fetchData<FolderWire>(`/elitea_core/folder/prompt_lib/${String(projectId)}`, {
    method: 'POST',
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });
  return normaliseFolder(wire);
}

/** Same flat `TAG_TYPE_FOLDERS`/`TAG_TYPE_TOTAL_FOLDERS` invalidation as `useFolderUpdateMutation`/`useDeleteFolderMutation` below (`conversationList.api.js:34`) — found missing by adversarial verify. */
export function useFolderCreateMutation(): UseMutationResult<Folder, unknown, FolderCreateParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: folderCreate,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: [...FOLDER_QUERY_ROOT, 'list'] }),
  });
}

/* ── foldersList — GET elitea_core/folder/prompt_lib/{projectId}?{...params}&grouped=true ── */
/* manifest: folder.list */

export interface FoldersListParams {
  readonly projectId: string | number;
  readonly params?: Readonly<Record<string, string | number | boolean | undefined>>;
}

function foldersListQueryString(params: FoldersListParams['params']): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params ?? {})) {
    if (value !== undefined) query.set(key, String(value));
  }
  // `grouped` is always forced true, matching conversationList.api.js:46-53 (spreads `params` first, then hardcodes `grouped: true` last).
  query.set('grouped', 'true');
  return `?${query.toString()}`;
}

export async function foldersList(params: FoldersListParams, signal?: AbortSignal): Promise<GroupedFoldersResponse> {
  const url = `/elitea_core/folder/prompt_lib/${String(params.projectId)}${foldersListQueryString(params.params)}`;
  const wire = await fetchData<GroupedFoldersResponseWire>(url, signal ? { signal } : {});
  return normaliseGroupedFoldersResponse(wire);
}

export function useFoldersListQuery(params: FoldersListParams, options: { enabled?: boolean } = {}): UseQueryResult<GroupedFoldersResponse> {
  return useQuery({
    queryKey: [...FOLDER_QUERY_ROOT, 'list', params.projectId, params.params],
    queryFn: ({ signal }) => foldersList(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── folderConversations — GET elitea_core/folder/prompt_lib/{projectId}?grouped=true&folder_id&limit&offset&sort_by&sort_order ── */
/* manifest: folder.conversationsByFolder */
/* Plain async fetcher, no useQuery hook — the old app only ever triggered this imperatively (`useLazyFolderConversationsQuery`, `Conversations.jsx:97,204-211`), same on-demand-pagination precedent as `conversationApi.ts`'s `messageList`. */

export interface FolderConversationsParams {
  readonly projectId: string | number;
  readonly folderId: string | number;
  readonly limit?: number;
  readonly offset?: number;
  readonly sort_by?: string;
  readonly sort_order?: string;
}

function folderConversationsQueryString(params: FolderConversationsParams): string {
  const query = new URLSearchParams({
    grouped: 'true',
    folder_id: String(params.folderId),
    limit: String(params.limit ?? 10),
    offset: String(params.offset ?? 0),
  });
  if (params.sort_by !== undefined) query.set('sort_by', params.sort_by);
  if (params.sort_order !== undefined) query.set('sort_order', params.sort_order);
  return `?${query.toString()}`;
}

export async function folderConversations(params: FolderConversationsParams, signal?: AbortSignal): Promise<FolderConversationsPage> {
  const url = `/elitea_core/folder/prompt_lib/${String(params.projectId)}${folderConversationsQueryString(params)}`;
  const wire = await fetchData<FolderConversationsPageWire>(url, signal ? { signal } : {});
  return normaliseFolderConversationsPage(wire);
}

/* ── dateGroupConversations — GET elitea_core/folder/prompt_lib/{projectId}?grouped=true&date_group&limit&offset&sort_by&sort_order ── */
/* manifest: folder.conversationsByDateGroup */
/* Same shape as folderConversations, keyed by date_group instead of folder_id — also a plain async fetcher, no hook (`useLazyDateGroupConversationsQuery`, `Conversations.jsx:97,141-148`). */

export interface DateGroupConversationsParams {
  readonly projectId: string | number;
  readonly dateGroup: string;
  readonly limit?: number;
  readonly offset?: number;
  readonly sort_by?: string;
  readonly sort_order?: string;
}

function dateGroupConversationsQueryString(params: DateGroupConversationsParams): string {
  const query = new URLSearchParams({
    grouped: 'true',
    date_group: params.dateGroup,
    limit: String(params.limit ?? 10),
    offset: String(params.offset ?? 0),
  });
  if (params.sort_by !== undefined) query.set('sort_by', params.sort_by);
  if (params.sort_order !== undefined) query.set('sort_order', params.sort_order);
  return `?${query.toString()}`;
}

export async function dateGroupConversations(params: DateGroupConversationsParams, signal?: AbortSignal): Promise<FolderConversationsPage> {
  const url = `/elitea_core/folder/prompt_lib/${String(params.projectId)}${dateGroupConversationsQueryString(params)}`;
  const wire = await fetchData<FolderConversationsPageWire>(url, signal ? { signal } : {});
  return normaliseFolderConversationsPage(wire);
}

/* ── folderUpdate — PUT elitea_core/folder/prompt_lib/{projectId}/{id} ── */
/* manifest: folder.update */

export interface FolderUpdateParams {
  readonly projectId: string | number;
  readonly id: string | number;
  readonly [key: string]: unknown;
}

export async function folderUpdate(params: FolderUpdateParams): Promise<unknown> {
  const { projectId, id, ...body } = params;
  return fetchData<unknown>(`/elitea_core/folder/prompt_lib/${String(projectId)}/${String(id)}`, {
    method: 'PUT',
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });
}

/** Invalidates every cached `foldersList` query (any projectId/params) — matching the old app's flat, unparameterised `TAG_TYPE_FOLDERS` tag (`conversationList.api.js:109-116`), not just the mutated folder's own project. */
export function useFolderUpdateMutation(): UseMutationResult<unknown, unknown, FolderUpdateParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: folderUpdate,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: [...FOLDER_QUERY_ROOT, 'list'] }),
  });
}

/* ── deleteFolder — DELETE elitea_core/folder/prompt_lib/{projectId}/{id} ── */
/* manifest: folder.delete */

export interface DeleteFolderParams {
  readonly projectId: string | number;
  readonly id: string | number;
}

export async function deleteFolder(params: DeleteFolderParams): Promise<unknown> {
  return fetchData<unknown>(`/elitea_core/folder/prompt_lib/${String(params.projectId)}/${String(params.id)}`, { method: 'DELETE' });
}

/** Same flat `TAG_TYPE_FOLDERS`/`TAG_TYPE_TOTAL_FOLDERS` invalidation as `useFolderUpdateMutation` above (`conversationList.api.js:125`). */
export function useDeleteFolderMutation(): UseMutationResult<unknown, unknown, DeleteFolderParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: deleteFolder,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: [...FOLDER_QUERY_ROOT, 'list'] }),
  });
}

/* ── folderPinUpdate — PATCH elitea_core/folder/prompt_lib/{projectId}/{id} ── */
/* manifest: folder.pinUpdate */

export interface FolderPinUpdateParams {
  readonly projectId: string | number;
  readonly id: string | number;
  readonly is_pinned: boolean;
}

export async function folderPinUpdate(params: FolderPinUpdateParams): Promise<unknown> {
  const { projectId, id, is_pinned } = params;
  return fetchData<unknown>(`/elitea_core/folder/prompt_lib/${String(projectId)}/${String(id)}`, {
    method: 'PATCH',
    headers: JSON_HEADERS,
    body: JSON.stringify({ is_pinned }),
  });
}

/**
 * NO `onSuccess` invalidation — deliberate, matching the old app's own
 * `invalidatesTags: []` for this exact endpoint (`conversationList.api.js:136`):
 * the caller (`useEditFolder.js:47-56` `onPinFolder`) does its own optimistic
 * local `meta.is_pinned` update instead of relying on a foldersList refetch.
 */
export function useFolderPinUpdateMutation(): UseMutationResult<unknown, unknown, FolderPinUpdateParams> {
  return useMutation({ mutationFn: folderPinUpdate });
}
