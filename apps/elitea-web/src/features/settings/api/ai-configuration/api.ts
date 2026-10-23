/**
 * Fetcher for GET/POST /configurations/models/{projectId}.
 * Ported from `apps/elitea-ui/src/api/configurations.js`'s `listModels` and
 * `setProjectDefaultModel` RTK Query endpoints. Neither has an OpenAPI
 * entry — shapes observed from request/response.
 *
 * DEFECT, fixed here: both calls used `window.fetch` with a bare
 * `/configurations/models/...` path. The shared HTTP client adds the
 * `/api/v2` base; a raw `fetch` does not. Every read and every write of a
 * project default model therefore answered 404. The read fell back to
 * `EMPTY_MODELS_RESPONSE`, so each "Default model" select rendered blank,
 * and the write failed with nothing but a `console.error`.
 *
 * Both calls now go through `eliteaFetch`, which resolves the base, sends
 * the same credentials as every other request, and rejects with a typed
 * `EliteaApiError`. Pass the path WITHOUT the `/api/v2` prefix — `http.ts`
 * adds it, so a hand-written prefix yields `/api/v2/api/v2/...`.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import type { ModelInfo } from '@/entities/credential';
import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';

/**
 * `eliteaFetch` resolves to orval's `{data, status, headers}` envelope, never
 * to the bare body. Unwrap it in one place — the same helper shape
 * `features/chat-input/api/models.ts` and `features/credentials/api/
 * configurations.ts` already use for this route. Reading the envelope as the
 * body makes every field read `undefined`.
 */
async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

/**
 * The low-tier/high-tier fields are optional on the wire. The sibling client
 * for the same route (`features/chat-input/api/models.ts`) carries no tier
 * fields at all, and a project with no tier default set gets none back.
 */
export interface ModelsApiResponse {
  items: readonly ModelInfo[];
  total: number;
  default_model_name?: string;
  default_model_project_id?: string;
  low_tier_default_model_name?: string;
  low_tier_default_model_project_id?: string;
  high_tier_default_model_name?: string;
  high_tier_default_model_project_id?: string;
}

/** Safe default shape — mirrors the old app's `useListModelsQuery` default arg. */
export const EMPTY_MODELS_RESPONSE: ModelsApiResponse = {
  items: [],
  total: 0,
  default_model_name: '',
  default_model_project_id: '',
  low_tier_default_model_name: '',
  low_tier_default_model_project_id: '',
  high_tier_default_model_name: '',
  high_tier_default_model_project_id: '',
};

/**
 * The server's own message for a failed model call.
 *
 * `EliteaApiError.message` is `eliteaFetch: 404 from <url>` — a URL is not an
 * explanation for a user. `HttpFailure.body` holds the parsed error body, so
 * prefer its `error`/`message` field and fall back to the generic message.
 */
export function modelConfigurationErrorMessage(error: unknown): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const { body } = error.failure;
    if (typeof body === 'string' && body !== '') return body;
    if (typeof body === 'object' && body !== null) {
      const record = body as Record<string, unknown>;
      const detail = record['error'] ?? record['message'];
      if (typeof detail === 'string' && detail !== '') return detail;
    }
  }
  if (error instanceof Error) return error.message;
  return String(error);
}

/**
 * Fetch model list for a project.
 * @param projectId — project UID from the selected project.
 * @param section — one of: `llm`, `embedding`, `image_generation`, `asr`, `tts`.
 * @param includeShared — include shared (public-project) models.
 * @param signal — react-query's abort signal, so the parallel section
 *   queries stop when the panel unmounts.
 */
async function fetchModels(
  projectId: string,
  section: string,
  includeShared: boolean,
  signal?: AbortSignal,
): Promise<ModelsApiResponse> {
  const params = new URLSearchParams({
    section,
    include_shared: String(includeShared),
  });

  return fetchData<ModelsApiResponse>(
    `/configurations/models/${projectId}?${params.toString()}`,
    signal ? { signal } : {},
  );
}

/**
 * `includeShared` belongs in the key: it changes the response, so two callers
 * that ask for the same project and section with a different value must not
 * share one cache entry.
 */
function modelsQueryKey(projectId: string, section: string, includeShared: boolean): readonly unknown[] {
  return ['settings', 'ai-configuration', 'models', projectId, section, includeShared];
}

/**
 * React Query wrapper around `fetchModels` — mirrors the old app's
 * `useListModelsQuery({ projectId, include_shared, section })`, used to
 * read the project's real configured default model per section (and, for
 * `section: 'llm'`, each model's real `default` flag).
 */
export function useModelsQuery(
  projectId: string,
  section: string,
  includeShared: boolean,
): UseQueryResult<ModelsApiResponse, Error> {
  return useQuery({
    queryKey: modelsQueryKey(projectId, section, includeShared),
    queryFn: ({ signal }) => fetchModels(projectId, section, includeShared, signal),
    enabled: !!projectId,
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}

export interface SetProjectDefaultModelParams {
  readonly name: string;
  readonly targetProjectId: string;
  /** e.g. `llm`, `llm_low_tier`, `llm_high_tier`, `embedding`, `image_generation`, `asr`, `tts`. */
  readonly section: string;
}

/**
 * Persist the project's default model for a section.
 * Ported from `apps/elitea-ui/src/api/configurations.js`'s
 * `setProjectDefaultModel` RTK Query mutation.
 */
async function setProjectDefaultModel(
  projectId: string,
  params: SetProjectDefaultModelParams,
): Promise<ModelsApiResponse> {
  return fetchData<ModelsApiResponse>(`/configurations/models/${projectId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      name: params.name,
      target_project_id: params.targetProjectId,
      section: params.section,
    }),
  });
}

/**
 * Mutation hook for `setProjectDefaultModel`. Invalidates every cached
 * models query for the project on success so the Select/"Default" badge
 * reflect the new default immediately, mirroring the old app's
 * `invalidatesTags: [TAG_MODELS]`.
 *
 * There is no `onError` here on purpose. A hook-level handler serves all
 * eight selects at once, so it cannot tell the caller WHICH section failed.
 * The caller passes its own `onError` to `mutate` and shows the message
 * beside the select that failed.
 */
export function useSetProjectDefaultModelMutation(
  projectId: string,
): UseMutationResult<ModelsApiResponse, Error, SetProjectDefaultModelParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (params) => setProjectDefaultModel(projectId, params),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['settings', 'ai-configuration', 'models', projectId] });
    },
  });
}
