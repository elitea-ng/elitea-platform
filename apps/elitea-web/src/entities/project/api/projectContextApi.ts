/**
 * Hand-written REST client for the project context & project info endpoints
 * (settings section).
 *
 * ## Generated transport, hand-written cache policy (issue 36, item 5)
 *
 * The project-info pair and all three project-icon routes are described in
 * `services/elitea-main/api/openapi/v2.yaml`, so the URLs, the multipart
 * assembly and the response types come from
 * `shared/api/generated/applications` and are not re-derived here. This module
 * keeps only what orval does not generate usefully: it shapes EVERY operation
 * as a `useQuery` gated by `enabled`, including the three writes, so the
 * mutations below wrap the generated FETCHERS and own the invalidation.
 *
 *   - `GET/PUT /elitea_core/project_info/prompt_lib/{projectId}/project-info`
 *     → `getProjectInfo` / `updateProjectInfo`
 *   - `GET/POST/DELETE /elitea_core/project_icon/prompt_lib/{projectId}`
 *     → `listProjectIcons` / `uploadProjectIcon` / `deleteProjectIcon`
 *   - `POST /elitea_core/generate_project_context_draft/prompt_lib/{projectId}`
 *     → still hand-written; it is item 3 of the same issue and undescribed.
 *
 * Source: `apps/elitea-ui/src/api/projectContext.js`,
 * `.../api/generateProjectContextDraftApi.js`,
 * `.../features/settings/api/projectInfoApi.js` — RTK Query endpoints.
 *
 * Per R-A5, every endpoint below is documented with a `manifest:` comment.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import {
  deleteProjectIcon as deleteProjectIconRequest,
  getProjectInfo as getProjectInfoRequest,
  listProjectIcons as listProjectIconsRequest,
  updateProjectInfo as updateProjectInfoRequest,
  uploadProjectIcon as uploadProjectIconRequest,
} from '@/shared/api/generated/applications/applications';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapListPage } from '@/shared/api/unwrap';

/* ── transport helpers ─────────────────────────────────────────────────── */

/**
 * `eliteaFetch<T>` ALWAYS resolves the mutator's `{data, status, headers}`
 * envelope, so the body is `envelope.data` — the same one-line helper every
 * sibling hand-written REST module here already uses (`conversationApi.ts`,
 * `secretApi.ts`, `foldersApi.ts`, …).
 *
 * Every fetcher below used to type the call as `eliteaFetch<XResponse>` and
 * return the result verbatim, i.e. it returned the ENVELOPE typed as the body.
 * `projectInfo?.icon_meta` and `?.teammates_count` were therefore permanently
 * `undefined` (no uploaded project icon ever rendered, the teammates count was
 * always 0), the uploaded-icons grid was permanently empty, and the generated
 * context draft came back blank — all with 200s and nothing in the console.
 * Same defect as the PAT that rendered blank in #132; found while migrating
 * the call sites for that issue.
 */
async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

interface ProjectInfoResponse {
  name?: string;
  icon_meta?: { name: string; url: string } | null;
  teammates_count?: number;
}

interface IconMetaRequest {
  name?: string | null;
  url?: string | null;
}

interface IconUploadResponse {
  name?: string;
  url?: string;
  /**
   * The three presentational keys social_save_image has always returned and
   * this route used to drop. They are optional here because a deployment on an
   * older elitea-main still answers two keys.
   */
  size?: string;
  initial_file_size?: string;
  resulting_file_size?: string;
}

/* ── query/mutation keys ───────────────────────────────────────────────── */

function projectInfoQueryKey(projectId: string): string[] {
  return ['project', 'info', projectId];
}

function projectIconsQueryKey(projectId: string): string[] {
  return ['project', 'icons', projectId];
}

function projectContextQueryKey(projectId: string): string[] {
  return ['project', 'context', projectId];
}

/* ── projectInfo — GET /project_info/prompt_lib/{projectId}/project-info ── */
/* manifest: projectInfo.get */

export async function fetchProjectInfo(projectId: string): Promise<ProjectInfoResponse> {
  const response = await getProjectInfoRequest(projectId);
  return (response as { data: ProjectInfoResponse }).data;
}

export function useProjectInfoQuery(
  projectId: string,
  options: { enabled?: boolean } = {},
): UseQueryResult<ProjectInfoResponse, Error> {
  return useQuery({
    queryKey: projectInfoQueryKey(projectId),
    queryFn: () => fetchProjectInfo(projectId),
    enabled: options.enabled ?? !!projectId,
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}

/* ── updateProjectInfo — PUT /project_info/prompt_lib/{projectId}/project-info */
/* manifest: projectInfo.update */

export async function updateProjectInfo(
  projectId: string,
  icon_meta: IconMetaRequest | null,
): Promise<ProjectInfoResponse> {
  // `icon_meta` is sent EXPLICITLY, null included: an absent key leaves the
  // icon alone and an explicit null clears it, which is the whole point of this
  // call site. The generated request type spells both out.
  const response = await updateProjectInfoRequest(projectId, { icon_meta });
  return (response as { data: ProjectInfoResponse }).data;
}

export function useUpdateProjectInfoMutation(projectId: string): UseMutationResult<ProjectInfoResponse, Error, IconMetaRequest | null> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (icon_meta) => updateProjectInfo(projectId, icon_meta),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: projectInfoQueryKey(projectId) }),
  });
}

/* ── getProjectIcons — GET /project_icon/prompt_lib/{projectId} ─────────── */
/* manifest: projectIcons.list */

interface UploadedIcon {
  name: string;
  url: string;
}

interface ProjectIconsResponse {
  rows: UploadedIcon[];
  total: number;
}

export async function fetchProjectIcons(
  projectId: string,
  _page = 0,
  pageSize = 200,
): Promise<ProjectIconsResponse> {
  // The list body is unwrapped by the one helper (R-A6, #132) — this endpoint
  // answers `{rows,total}`, but that is no longer something this call site has
  // to know, assert, or copy correctly.
  const response = await listProjectIconsRequest(projectId, { limit: pageSize, skip: _page * pageSize });
  return unwrapListPage<UploadedIcon>(response, 'projectIcons.list');
}

export function useProjectIconsQuery(
  projectId: string,
  options: { enabled?: boolean } = {},
): UseQueryResult<ProjectIconsResponse, Error> {
  return useQuery({
    queryKey: projectIconsQueryKey(projectId),
    queryFn: () => fetchProjectIcons(projectId),
    enabled: options.enabled ?? !!projectId,
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}

/* ── uploadProjectIcon — POST /project_icon/prompt_lib/{projectId} ──────── */
/* manifest: projectIcons.upload */

export interface UploadIconParams {
  file: File;
  width?: number;
  height?: number;
}

export async function uploadProjectIcon(
  projectId: string,
  params: UploadIconParams,
): Promise<IconUploadResponse> {
  const response = await uploadProjectIconRequest(projectId, {
    file: params.file,
    ...(params.width ? { width: params.width } : {}),
    ...(params.height ? { height: params.height } : {}),
  });
  return (response as { data: IconUploadResponse }).data;
}

export function useUploadProjectIconMutation(
  projectId: string,
): UseMutationResult<IconUploadResponse, Error, UploadIconParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (params) => uploadProjectIcon(projectId, params),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: projectIconsQueryKey(projectId) });
      void queryClient.invalidateQueries({ queryKey: projectInfoQueryKey(projectId) });
    },
  });
}

/* ── deleteProjectIcon — DELETE /project_icon/prompt_lib/{projectId}/{name} */
/* manifest: projectIcons.delete */

export async function deleteProjectIcon(
  projectId: string,
  name: string,
): Promise<void> {
  // ENCODED HERE, not by the generated URL builder: orval interpolates a path
  // parameter verbatim, and a stored icon name really can contain a space.
  await deleteProjectIconRequest(projectId, encodeURIComponent(name));
}

export function useDeleteProjectIconMutation(
  projectId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name) => deleteProjectIcon(projectId, name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: projectIconsQueryKey(projectId) });
    },
  });
}

/* ── generateProjectContextDraft — POST /generate_project_context_draft/prompt_lib/{projectId} */
/* manifest: draft.generate */

export interface GenerateDraftParams {
  user_description?: string;
}

export interface DraftResponse {
  project_background?: string;
}

export async function generateProjectContextDraft(
  projectId: string,
  params: GenerateDraftParams,
): Promise<DraftResponse> {
  const resp = await fetchData<DraftResponse>(
    `/elitea_core/generate_project_context_draft/prompt_lib/${projectId}`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(params),
    },
  );
  return resp;
}

export function useGenerateProjectContextDraftMutation(
  projectId: string,
): UseMutationResult<DraftResponse, Error, GenerateDraftParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (params) => generateProjectContextDraft(projectId, params),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: projectContextQueryKey(projectId) });
    },
  });
}
