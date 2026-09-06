/**
 * Hand-written REST client for the configurations domain (settings:
 * environment variables + service prompts).
 *
 * Source: `apps/elitea-ui/src/api/configurations.js` — RTK Query endpoints
 * (`getConfigurationsList` / `getAvailableConfigurationsType` /
 * `createConfiguration` / `updateConfiguration`). Every route maps to real
 * Go handlers. The list path is `/configurations/configurations/{project_id}`.
 * The detail path is `/configurations/configuration/{project_id}/{config_id}`.
 *
 * The generated `admin.ts` does NOT include these endpoints (they live on
 * a different route namespace), so this is a handwritten client following
 * the same shape as `entities/secret/api/secretApi.ts`.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

/**
 * `eliteaFetch` resolves to the ENVELOPED shape `{data, status, headers}`,
 * never the bare body (`generated/mutator.ts`'s own contract comment). The
 * three readers below returned that envelope straight to their callers, so
 * `response.items` / `response.default_model_name` were always `undefined`:
 * `useConfigurationsBySection` read `q.data.items` on all seven sections and
 * got nothing, and the AI-Configuration page rendered no configuration card
 * at all — a saved credential was invisible however correct the row was
 * (#131). `features/credentials/api/configurations.ts` already unwraps at
 * exactly one place for this reason; this is the same helper.
 */
async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

/* ── type shapes ──────────────────────────────────────────────────────── */

/** A single configuration item returned by the server. */
export interface ConfigurationItem {
  readonly id: number;
  /**
   * A NUMBER on this route (`CurrentConfigurationDTO.ProjectID` is `int32`).
   * It was declared `string` here, and nothing checks a declaration against
   * the wire, so `configuration.project_id === projectId` silently compared a
   * number with a string and every AI-configuration card reported "No edit
   * permissions". Declared as the union it really is, so a caller has to
   * normalise (`String()`) instead of assuming.
   */
  readonly project_id: string | number;
  readonly elitea_title: string;
  readonly label: string;
  readonly type: string;
  readonly section: string;
  readonly shared: boolean;
  readonly data: Record<string, unknown>;
  readonly created_at?: string;
  readonly updated_at?: string;
}

/** Paginated list response envelope. */
export interface ConfigurationsListResponse {
  items: ConfigurationItem[];
  total: number;
  offset: number;
  limit: number;
}

/** Schema returned by the available-configurations-type endpoint. */
export interface AvailableConfigurationType {
  type: string;
  config_schema?: Record<string, unknown>;
}

/** Params for `getConfigurationsList`. */
export interface ConfigurationsListParams {
  readonly projectId: string;
  readonly section: string;
  readonly includeShared?: boolean;
  readonly pageSize?: number;
}

/** Params for `getAvailableConfigurationsType`. */
export interface AvailableConfigurationsTypeParams {
  readonly section: string;
}

/** Body shape for `createConfiguration`. */
export interface CreateConfigurationBody {
  readonly elitea_title: string;
  readonly label: string;
  readonly type: string;
  readonly shared: boolean;
  readonly data: Record<string, unknown>;
}

/** Body shape for `updateConfiguration`. */
export interface UpdateConfigurationBody {
  readonly label: string;
  readonly shared: boolean;
  readonly data: Record<string, unknown>;
}

/* ── transport helpers ────────────────────────────────────────────────── */

/* The Go handler mounts at /configurations with inner routes /configurations/…
   (handler.go:37-40), so the full API path is /configurations/configurations/{project_id}.
   The old app does NOT use a mode prefix for configurations. */
function configurationsPath(projectId: string): string {
  return `/configurations/configurations/${projectId}`;
}

/* The DETAIL path is singular and carries both identifiers:
   `/configurations/configuration/{project_id}/{config_id}`
   (CurrentConfigurationDetailsPath in
   services/elitea-main/internal/api/v2/configurations/read.go).
   PUT and DELETE are registered on this path only. */
function configurationDetailPath(projectId: string, configId: string): string {
  return `/configurations/configuration/${projectId}/${configId}`;
}

/* The MODEL CATALOGUE, which is a different route from the configuration list
   above. `/configurations/configurations/{id}?section=models` returns
   CREDENTIAL rows (elitea_title / label / data); this one returns models
   (`name`, `display_name`, `context_window`, `supports_reasoning`) — the shape
   `ConfigModel` declares, field for field. */
function modelCatalogPath(projectId: string): string {
  return `/configurations/models/${projectId}`;
}

function availableTypesPath(): string {
  return `/configurations/available/`;
}

/* ── query keys ───────────────────────────────────────────────────────── */

function configurationsListKey(params: ConfigurationsListParams): string[] {
  return ['settings', 'configurations', params.projectId, params.section];
}

function availableTypesKey(section: string): string[] {
  return ['settings', 'availableTypes', section];
}

/* ── getConfigurationsList — GET /configurations/configurations/{project_id} */
/* manifest: configurations.list */

export async function getConfigurationsList(
  params: ConfigurationsListParams,
): Promise<ConfigurationsListResponse> {
  const qs = new URLSearchParams({
    section: params.section,
    include_shared: String(params.includeShared ?? false),
    limit: String(params.pageSize ?? 100),
    offset: '0',
    sort_by: 'created_at',
    sort_order: 'desc',
  });
  return fetchData<ConfigurationsListResponse>(
    `${configurationsPath(params.projectId)}?${qs}`,
  );
}

export function useGetConfigurationsListQuery(
  params: ConfigurationsListParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<ConfigurationsListResponse, Error> {
  return useQuery({
    queryKey: configurationsListKey(params),
    queryFn: () => getConfigurationsList(params),
    enabled: options.enabled ?? true,
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}

/* ── getAvailableConfigurationsType — GET /configurations/available/?section=... */
/* manifest: configurations.availableTypes */

export async function getAvailableConfigurationsType(
  params: AvailableConfigurationsTypeParams,
): Promise<AvailableConfigurationType[]> {
  const qs = new URLSearchParams({ section: params.section });
  return fetchData<AvailableConfigurationType[]>(
    `${availableTypesPath()}?${qs}`,
  );
}

export function useGetAvailableConfigurationsTypeQuery(
  params: AvailableConfigurationsTypeParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<AvailableConfigurationType[], Error> {
  return useQuery({
    queryKey: availableTypesKey(params.section),
    queryFn: () => getAvailableConfigurationsType(params),
    enabled: options.enabled ?? true,
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}

/* ── createConfiguration — POST /configurations/configurations/{project_id} */
/* manifest: configurations.create */

export async function createConfiguration(
  projectId: string,
  body: CreateConfigurationBody,
): Promise<void> {
  await eliteaFetch<unknown>(configurationsPath(projectId), {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useCreateConfigurationMutation(
  projectId: string,
): UseMutationResult<void, Error, CreateConfigurationBody> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body) => createConfiguration(projectId, body),
    onSuccess: () => {
      // Invalidate all configuration queries for this project
      void queryClient.invalidateQueries({ queryKey: ['settings', 'configurations'] });
      void queryClient.invalidateQueries({ queryKey: ['settings', 'availableTypes'] });
    },
  });
}

/* ── updateConfiguration — PUT /configurations/configuration/{project_id}/{config_id} ── */
/* manifest: configurations.update */

/** Arguments the caller gives to the update mutation. */
export interface UpdateConfigurationArgs {
  readonly configId: string;
  readonly body: UpdateConfigurationBody;
}

/** Arguments the update request needs. The hook adds the project id. */
export interface UpdateConfigurationRequest extends UpdateConfigurationArgs {
  readonly projectId: string;
}

export async function updateConfiguration(
  { projectId, configId, body }: UpdateConfigurationRequest,
): Promise<void> {
  await eliteaFetch<unknown>(
    configurationDetailPath(projectId, configId),
    {
      method: 'PUT',
      body: JSON.stringify(body),
      headers: { 'Content-Type': 'application/json' },
    },
  );
}

export function useUpdateConfigurationMutation(
  projectId: string,
): UseMutationResult<void, Error, UpdateConfigurationArgs> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (args: UpdateConfigurationArgs) =>
      updateConfiguration({ projectId, configId: args.configId, body: args.body }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['settings', 'configurations', projectId] });
      void queryClient.invalidateQueries({ queryKey: ['settings', 'availableTypes'] });
    },
  });
}

/* ── listModels — GET /configurations/configurations/{project_id}?section=models ── */
/* manifest: configurations.listModels */

/**
 * Model entry returned by the configurations endpoint when queried for models.
 * Mirrors the shape returned by `useListModelsQuery` from the old app's
 * `@/api/configurations.js`.
 */
export interface ConfigModel {
  readonly name: string;
  readonly project_id: string;
  readonly default?: boolean;
  readonly display_name?: string;
  readonly id?: string | number;
  readonly uid?: string;
  [key: string]: unknown;
}

/** Response envelope for the models list query. */
export interface ConfigModelsListResponse {
  items: ConfigModel[];
  default_model_name: string;
}

export interface ListModelsParams {
  readonly projectId: string;
  readonly include_shared?: boolean;
}

/**
 * The models a project can run — what the chat composer's picker offers.
 *
 * Reads the MODEL CATALOGUE, not the configuration list. It used to call
 * `/configurations/configurations/{id}?section=models`, which returns model
 * CREDENTIALS: rows carrying `elitea_title`/`label`/`data` and no `name` at
 * all. Every consumer here reads `.name`, so the picker rendered rows with an
 * EMPTY label — a menu of entries nobody can identify — and anything that did
 * choose one sent a credential's title where the backend expects a model alias,
 * which it rejects (#293).
 *
 * The catalogue answers `{name, display_name, project_id, shared,
 * context_window, max_output_tokens, supports_reasoning}` — `ConfigModel`'s
 * declared shape, field for field, which is the evidence this was always the
 * intended source.
 */
export async function listModels(
  params: ListModelsParams,
): Promise<ConfigModelsListResponse> {
  const qs = new URLSearchParams({ include_shared: String(params.include_shared ?? false) });
  return fetchData<ConfigModelsListResponse>(`${modelCatalogPath(params.projectId)}?${qs}`);
}

export function useListModelsQuery(
  params: ListModelsParams,
  options?: { enabled?: boolean; skip?: boolean },
): UseQueryResult<ConfigModelsListResponse, Error> {
  return useQuery({
    queryKey: ['models', params.projectId],
    queryFn: () => listModels(params),
    enabled: options?.enabled ?? (options?.skip ? !options.skip : true),
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}
