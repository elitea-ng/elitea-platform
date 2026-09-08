/**
 * Skill icon gallery, upload, bind and delete.
 *
 * Baseline: `apps/elitea-ui/src/[fsd]/features/skill/api/skillsApi.js`
 * (`getSkillIcons` / `uploadSkillIcon` / `replaceSkillIcon` / `deleteSkillIcon`)
 * — parity manifest API-070 … API-073.
 *
 * ## Generated transport, hand-written cache policy (issue 36, item 1)
 *
 * The five routes are described in `services/elitea-main/api/openapi/v2.yaml`,
 * so the URL building, the multipart assembly and the response types come from
 * `shared/api/generated/skills` and are no longer re-derived here. What stays
 * hand-written is the part orval does not generate usefully: it shapes EVERY
 * operation as a `useQuery` gated by `enabled`, including the three writes, so
 * the mutations below wrap the generated FETCHERS in `useMutation` and own the
 * invalidation. That is the same split `entities/application-form/model/
 * mutations.ts` established for this codebase.
 *
 * ONE SHAPE NOTE THAT IS LOAD-BEARING. `eliteaFetch` resolves the
 * `{data, status, headers}` envelope, and every generated fetcher returns it —
 * so a call site that treats the result as the BODY gets `undefined` fields on
 * a 200, the #132 defect, which shows as an empty gallery with nothing in the
 * console. The list unwrap goes through the one sanctioned helper (R-A6).
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import {
  bindSkillIcon as bindSkillIconRequest,
  deleteSkillIcon as deleteSkillIconRequest,
  listSkillIcons as listSkillIconsRequest,
  uploadSkillIcon as uploadSkillIconRequest,
  uploadSkillIconForVersion as uploadSkillIconForVersionRequest,
} from '@/shared/api/generated/skills/skills';
import { unwrapListPage } from '@/shared/api/unwrap';

/** One entry of the uploaded-icon gallery. */
interface SkillIcon {
  readonly name: string;
  readonly url: string;
}

/**
 * The payload the upload answers and the bind (PUT) sends back. `name` and
 * `url` are what the server requires; the rest is presentational metadata the
 * baseline carries through unchanged.
 */
export interface SkillIconMeta {
  readonly name: string;
  readonly url: string;
  readonly size?: string;
  readonly initial_file_size?: string;
  readonly resulting_file_size?: string;
}

export interface SkillIconsPage {
  readonly rows: readonly SkillIcon[];
  readonly total: number;
}

const skillIconQueryKey = (projectId: string): readonly string[] => ['skills', projectId, 'icons'];

/* ── list — GET /upload_skill_icon/prompt_lib/{projectId} ───────────────── */
/* manifest: skillIcons.list (listSkillIcons) — parity API-070 */

export async function fetchSkillIcons(
  projectId: string,
  page = 0,
  pageSize = 200,
): Promise<SkillIconsPage> {
  const response = await listSkillIconsRequest(projectId, { limit: pageSize, skip: page * pageSize });
  return unwrapListPage<SkillIcon>(response, 'skillIcons.list');
}

export function useSkillIconsQuery(
  projectId: string,
  options: { enabled?: boolean } = {},
): UseQueryResult<SkillIconsPage, Error> {
  return useQuery({
    queryKey: skillIconQueryKey(projectId),
    queryFn: () => fetchSkillIcons(projectId),
    enabled: options.enabled ?? !!projectId,
    refetchOnWindowFocus: false,
  });
}

/* ── upload — POST /upload_skill_icon/prompt_lib/{projectId}[/{versionId}] ─ */
/* manifest: skillIcons.upload / skillIcons.uploadForVersion — parity API-071 */

export interface UploadSkillIconParams {
  readonly file: File;
  readonly width?: number;
  readonly height?: number;
  /**
   * When present the icon is bound to that skill version by the SAME request.
   * The baseline's dynamic path template — the trailing segment is optional.
   */
  readonly versionId?: string;
}

export async function uploadSkillIcon(
  projectId: string,
  params: UploadSkillIconParams,
): Promise<SkillIconMeta> {
  const form = {
    file: params.file,
    ...(params.width !== undefined ? { width: params.width } : {}),
    ...(params.height !== undefined ? { height: params.height } : {}),
  };
  // Two DESCRIBED operations, not one path with an optional tail: the document
  // gives the bound upload its own path because the trailing segment is a
  // different operation with a 404 the two-segment form cannot answer.
  const response = params.versionId
    ? await uploadSkillIconForVersionRequest(projectId, Number(params.versionId), form)
    : await uploadSkillIconRequest(projectId, form);
  return (response as { data: SkillIconMeta }).data;
}

export function useUploadSkillIconMutation(
  projectId: string,
): UseMutationResult<SkillIconMeta, Error, UploadSkillIconParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (params) => uploadSkillIcon(projectId, params),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: skillIconQueryKey(projectId) });
      void queryClient.invalidateQueries({ queryKey: ['skills', projectId] });
    },
  });
}

/* ── bind — PUT /upload_skill_icon/prompt_lib/{projectId}/{versionId} ────── */
/* manifest: skillIcons.bind (bindSkillIcon) — parity API-072 */

export interface BindSkillIconParams {
  readonly versionId: string;
  /** `null` resets the version to the default icon, as the baseline's empty name/url pair does. */
  readonly iconMeta: SkillIconMeta | null;
}

export async function bindSkillIcon(
  projectId: string,
  params: BindSkillIconParams,
): Promise<void> {
  // The server requires `name` and `url` to be present strings; a reset sends
  // them empty rather than omitting them, which is exactly what pylon's
  // UpdateIcon model accepts.
  const body = params.iconMeta ?? { name: '', url: '' };
  await bindSkillIconRequest(projectId, Number(params.versionId), { ...body });
}

export function useBindSkillIconMutation(
  projectId: string,
): UseMutationResult<void, Error, BindSkillIconParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (params) => bindSkillIcon(projectId, params),
    // The skill detail and every list that renders the icon read
    // `version_details.meta.icon_meta`, so they are what a successful bind
    // changes — the gallery itself is unaffected.
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['skills', projectId] });
    },
  });
}

/* ── delete — DELETE /upload_skill_icon/prompt_lib/{projectId}/{name} ────── */
/* manifest: skillIcons.delete (deleteSkillIcon) — parity API-073 */

export async function deleteSkillIcon(projectId: string, name: string): Promise<void> {
  // ENCODED HERE, not by the generated URL builder. orval interpolates a path
  // parameter verbatim, and an icon name is a stored file name that really can
  // contain a space — an unencoded one would address a different path, or none.
  await deleteSkillIconRequest(projectId, encodeURIComponent(name));
}

export function useDeleteSkillIconMutation(
  projectId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name) => deleteSkillIcon(projectId, name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: skillIconQueryKey(projectId) });
      // The delete also unlinks the icon from every skill version wearing it,
      // so the skill reads are stale too. Not invalidating them is how a
      // deleted icon keeps rendering as a broken image until a hard reload.
      void queryClient.invalidateQueries({ queryKey: ['skills', projectId] });
    },
  });
}
