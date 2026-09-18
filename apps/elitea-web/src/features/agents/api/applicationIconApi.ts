/**
 * Agent (application) icon upload + bind, hand-written cache policy over the
 * generated transport — same split `features/skills/api/skillIconApi.ts`
 * already established for the sibling entity (issue 36, item 1: orval shapes
 * EVERY operation, including the three writes, as a `useQuery`; the mutations
 * below wrap the generated FETCHERS in `useMutation` and own invalidation).
 *
 * `elitea_issues: #6627` — the agent editor has never had an EDITABLE icon
 * (`ApplicationEditForm.tsx`'s and `EntityIcon.tsx`'s own doc comments both
 * disclose the gap: "a future unit that needs an EDITABLE entity icon should
 * build that mode fresh"). This is that unit's data layer.
 *
 * Two Go routes, not one: `POST .../upload_icon/prompt_lib/{projectId}`
 * (`internal/api/v2/eliteacore/handler.go:2556` `UploadIcon`) only stores the
 * file and returns its url — it does NOT touch `application_versions`. Binding
 * an icon (uploaded OR a default) to a specific version is the SEPARATE
 * `PUT .../upload_icon/prompt_lib/{projectId}/{versionId}` (`UpdateIcon`,
 * `handler.go:2729`), which stores the JSON body verbatim as
 * `meta.icon_meta`. A selection is therefore always upload-then-bind (for a
 * fresh file) or bind-only (for a default icon) — never upload alone.
 */
import {
  useMutation,
  useQueryClient,
  type UseMutationResult,
} from '@tanstack/react-query';

import {
  getGetApplicationQueryKey,
  replaceApplicationIcon as replaceApplicationIconRequest,
  uploadApplicationIcon as uploadApplicationIconRequest,
} from '@/shared/api/generated/applications/applications';

/** The stored shape of `application_versions.meta.icon_meta`. */
export interface ApplicationIconMeta {
  readonly name?: string | undefined;
  readonly url: string;
  readonly width?: number | undefined;
  readonly height?: number | undefined;
}

/* ── upload — POST /upload_icon/prompt_lib/{projectId} ───────────────────── */

export interface UploadApplicationIconParams {
  readonly file: File;
}

/** Uploads a file to storage and returns the ref the server minted for it — NOT yet bound to any version. */
export async function uploadApplicationIconFile(
  projectId: string,
  params: UploadApplicationIconParams,
): Promise<ApplicationIconMeta | null> {
  // `versionId` is a required positional arg of the generated fetcher (the
  // route also accepts an optional trailing segment), but `UploadIcon`'s own
  // Go body never reads it — only `projectID`. `0` is therefore inert, not a
  // real version reference; binding is the separate PUT below.
  const response = await uploadApplicationIconRequest(projectId, 0, { file: params.file });
  const body = (response as { data: { ok: boolean; url: string; icon_meta?: { url: string; width: number; height: number } } }).data;
  if (!body.icon_meta) return null; // the "no file" fast path — matches the baseline's own no-op
  return { url: body.icon_meta.url, width: body.icon_meta.width, height: body.icon_meta.height };
}

export function useUploadApplicationIconMutation(
  projectId: string,
): UseMutationResult<ApplicationIconMeta | null, Error, UploadApplicationIconParams> {
  return useMutation({
    mutationFn: (params) => uploadApplicationIconFile(projectId, params),
  });
}

/* ── bind — PUT /upload_icon/prompt_lib/{projectId}/{versionId} ──────────── */

export interface BindApplicationIconParams {
  readonly applicationId: number;
  readonly versionId: string;
  /** `null` resets the version to its per-type fallback glyph — an empty object clears every key `UpdateIcon` merged in previously. */
  readonly iconMeta: ApplicationIconMeta | null;
}

export async function bindApplicationIcon(projectId: string, params: BindApplicationIconParams): Promise<void> {
  const body: Record<string, unknown> = params.iconMeta ? { ...params.iconMeta } : {};
  await replaceApplicationIconRequest(projectId, Number(params.versionId), body);
}

export function useBindApplicationIconMutation(
  projectId: string,
): UseMutationResult<void, Error, BindApplicationIconParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (params) => bindApplicationIcon(projectId, params),
    // Matches `EditApplicationToolsPanel.tsx`'s own convention: the agent
    // editor reads the version's meta through `useEditApplicationData`'s
    // `getApplication` query, so invalidating anything else would leave the
    // header/list card showing the old icon until an unrelated refetch.
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, params.applicationId) });
    },
  });
}
