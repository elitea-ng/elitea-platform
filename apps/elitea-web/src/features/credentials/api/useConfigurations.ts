/**
 * features/credentials/api/useConfigurations.ts — TanStack Query hooks
 * wrapping `./configurations.ts` (unit A7). Substitutes for the baseline's
 * RTK Query cache: query-key based caching replaces `providesTags`, and
 * explicit `invalidateQueries` calls replace `invalidatesTags` (spec §2.3 —
 * "TanStack Query + zustand", no Redux/RTK Query anywhere in the new app).
 *
 * Query-key convention: `['credentials', 'configurations', projectId, ...]`
 * so `invalidateQueries({ queryKey: ['credentials', 'configurations'] })`
 * (no projectId) invalidates every list for every project at once — the
 * same "wipe the whole configurations surface" scope the baseline's shared
 * `TAG_CONFIGURATIONS`/`TAG_SHARED_CONFIGURATIONS` tags had.
 *
 * SCOPE NOTE: this file wraps the 8 of the 16 `./configurations.ts`
 * endpoints this unit's own `pages/credentials`/`features/credentials`
 * screens actually call (API-145/146/149/150/152/153/154/155). The
 * remaining 8 (API-147/148/151/156..160 —
 * `getConfigurationsByType`/`getConfigurationsBySection`/
 * `getSharedConfigurations`/`toggleConfigurationSharing`/`listModels`/
 * `listCredentialTypes`/`setProjectDefaultModel`/`getTtsVoices`) are fully
 * implemented AND independently contract-tested directly against
 * `./configurations.ts` (`configurations.test.ts` — satisfying API-147/
 * 148/151/156/157/158/159/160's manifest acceptance, which is about the
 * endpoint's request/response contract, not about a query-hook wrapper
 * existing). No hook wrapper is added for them here because they have no
 * real consumer yet within this unit's own pages (a hook with zero call
 * sites is untestable-as-a-hook and a dead export under `knip
 * --max-issues 0`, per this program's established discipline — see the
 * decision record's "First real CI run" section for the exact class of
 * problem this avoids). Add the wrapper back, mirroring the pattern below,
 * the moment a real caller needs one.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult, UseQueryOptions, UseQueryResult } from '@tanstack/react-query';

import { batchTestConfigurationConnection, testConfigurationConnection } from './configurationConnections';
import type { BatchTestConnectionItem, BatchTestResultRow } from './configurationConnections';
import {
  checkStoredConfigurationConnection,
  createConfiguration,
  deleteConfiguration,
  getAvailableConfigurationsType,
  getConfigurationDetail,
  getConfigurationsList,
  updateConfiguration,
} from './configurations';
import type {
  ConfigurationPageWire,
  ConfigurationTypeDescriptor,
  ConfigurationWire,
  CreateConfigurationBody,
  GetAvailableConfigurationsTypeParams,
  GetConfigurationsListParams,
  StoredConnectionCheckResult,
  UpdateConfigurationBody,
} from './configurations';

/**
 * Root key every credentials query/mutation shares — the invalidation
 * scope. Not exported (R-D1/knip): no caller outside this file needs to
 * invalidate credentials data directly today; export it the moment one
 * does, rather than carrying a currently-dead export.
 */
const CONFIGURATIONS_QUERY_ROOT = ['credentials', 'configurations'] as const;
const MODELS_QUERY_ROOT = ['credentials', 'models'] as const;

/**
 * The AI-Configuration screen reads the SAME server resource under a
 * different key namespace: `features/settings/lib/ai-configuration/
 * useConfigurationsBySection.ts` fires seven queries keyed
 * `['settings', 'configurations', projectId, section]`, and
 * `shared/api/configurationsApi.ts` keys the model list `['models', ...]`.
 * Invalidating only the `credentials` roots left every one of those caches
 * untouched — after saving a credential the app navigated straight back to
 * that screen and issued NO list request at all, so the row the POST had
 * just created was invisible until a full page reload (#131; measured: one
 * POST and zero GETs after save).
 *
 * Two namespaces over one resource is the actual defect; unifying them is a
 * cross-feature refactor. Until then every configuration mutation must
 * invalidate both, and this list is the one place that records why.
 */
const CONFIGURATION_RESOURCE_ROOTS = [
  CONFIGURATIONS_QUERY_ROOT,
  MODELS_QUERY_ROOT,
  ['settings', 'configurations'],
  ['settings', 'availableTypes'],
  ['models'],
] as const;

function invalidateConfigurationResource(queryClient: ReturnType<typeof useQueryClient>): void {
  for (const queryKey of CONFIGURATION_RESOURCE_ROOTS) {
    void queryClient.invalidateQueries({ queryKey });
  }
}

/* ── API-145 ───────────────────────────────────────────────────────────── */

export function useAvailableConfigurationsType(
  params: GetAvailableConfigurationsTypeParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<ConfigurationTypeDescriptor[]> {
  return useQuery({
    queryKey: [...CONFIGURATIONS_QUERY_ROOT, 'available', params],
    queryFn: ({ signal }) => getAvailableConfigurationsType(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── API-146 ───────────────────────────────────────────────────────────── */

export function useConfigurationsList(
  params: GetConfigurationsListParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<ConfigurationPageWire> {
  return useQuery({
    queryKey: [...CONFIGURATIONS_QUERY_ROOT, 'list', params],
    queryFn: ({ signal }) => getConfigurationsList(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── API-149 ───────────────────────────────────────────────────────────── */

export function useCreateConfiguration(): UseMutationResult<
  ConfigurationWire,
  Error,
  { projectId: string | number; body: CreateConfigurationBody }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, body }) => createConfiguration(projectId, body),
    onSuccess: () => {
      invalidateConfigurationResource(queryClient);
    },
  });
}

/* ── API-150 ───────────────────────────────────────────────────────────── */

/**
 * An id this hook may build a URL from.
 *
 * BLANK IS NOT READY, and `!== undefined` was not enough to say so. The
 * selected-project store starts at `null` and hydrates from storage in an
 * effect (`widgets/app-shell`'s `useSelectedProject`), and every consumer of
 * it reads `state.project?.id ?? ''` — so on the FIRST render of a deep link
 * such as `/settings/edit-configuration/{id}` the project id is the empty
 * string. `'' !== undefined`, so this query fired against
 * `/configurations/configuration//{id}`: a request for no project, answered
 * by nothing, and then abandoned when the store resolved and the query key
 * changed.
 *
 * Measured as an E2E failure rather than a screen defect (the second, real
 * read seeds the form, so the page looks right): journey J19b matched the
 * abandoned request, and chromium then has no body to give for it —
 * `Protocol error (Network.getResponseBody): No data found for resource with
 * given identifier`. Webkit kept the body and passed, which is exactly the
 * kind of engine-shaped signal a wasted request produces.
 */
function isResolvedId(id: string | number | undefined): boolean {
  return id !== undefined && String(id) !== '';
}

export function useConfigurationDetail(
  projectId: string | number | undefined,
  configId: string | number | undefined,
  options: Pick<UseQueryOptions<ConfigurationWire>, 'enabled'> = {},
): UseQueryResult<ConfigurationWire> {
  return useQuery({
    queryKey: [...CONFIGURATIONS_QUERY_ROOT, 'detail', projectId, configId],
    queryFn: ({ signal }) => getConfigurationDetail(projectId as string | number, configId as string | number, signal),
    enabled: (options.enabled ?? true) && isResolvedId(projectId) && isResolvedId(configId),
  });
}

/* ── API-152 ───────────────────────────────────────────────────────────── */

export function useUpdateConfiguration(): UseMutationResult<
  ConfigurationWire,
  Error,
  { projectId: string | number; configId: string | number; body: UpdateConfigurationBody }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, configId, body }) => updateConfiguration(projectId, configId, body),
    onSuccess: (_data, variables) => {
      invalidateConfigurationResource(queryClient);
      void queryClient.invalidateQueries({
        queryKey: [...CONFIGURATIONS_QUERY_ROOT, 'detail', variables.projectId, variables.configId],
      });
    },
  });
}

/* ── API-153 ───────────────────────────────────────────────────────────── */

export function useDeleteConfiguration(): UseMutationResult<
  unknown,
  Error,
  { projectId: string | number; configId: string | number }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, configId }) => deleteConfiguration(projectId, configId),
    onSuccess: () => {
      invalidateConfigurationResource(queryClient);
    },
  });
}

/* ── API-154 ───────────────────────────────────────────────────────────── */

export function useTestConfigurationConnection(): UseMutationResult<
  { readonly error?: string } & Record<string, unknown>,
  Error,
  { projectId: string | number; configType: string; body: Readonly<Record<string, unknown>> }
> {
  return useMutation({
    mutationFn: ({ projectId, configType, body }) => testConfigurationConnection(projectId, configType, body),
  });
}

/* ── API-155 ───────────────────────────────────────────────────────────── */

export function useBatchTestConfigurationConnection(): UseMutationResult<
  BatchTestResultRow[],
  Error,
  { projectId: string | number; items: readonly BatchTestConnectionItem[] }
> {
  return useMutation({
    mutationFn: ({ projectId, items }) => batchTestConfigurationConnection(projectId, items),
  });
}

/* ── stored-connection check (no API-* id — see `./configurations.ts`'s
   own "stored-connection-check family" comment for why this family has no
   baseline precedent) ─────────────────────────────────────────────────── */

/**
 * The SAVED-row form of `useTestConfigurationConnection`.
 *
 * The two are not interchangeable. `useTestConfigurationConnection` posts the
 * candidate payload — it can only test a credential whose secret the browser
 * still holds, which is true exactly once: while the user is typing it. Every
 * read path returns the stored secret sealed as a `{{secret.NAME}}`
 * reference, so re-testing a saved row through that route would ask the
 * provider to authenticate a literal template string. This hook posts NO BODY
 * AT ALL: the server reads the row, redeems the reference through the
 * project vault, and dials the provider itself.
 *
 * A failed check is HTTP 400 (`{success:false, message}`), so it arrives here
 * as a thrown `EliteaApiError`, not as a resolved `{success:false}` —
 * callers must read the message off `failure.body`.
 */
export function useCheckStoredConfigurationConnection(): UseMutationResult<
  StoredConnectionCheckResult,
  Error,
  { projectId: string | number; configId: string | number }
> {
  return useMutation({
    mutationFn: ({ projectId, configId }) => checkStoredConfigurationConnection(projectId, configId),
  });
}
