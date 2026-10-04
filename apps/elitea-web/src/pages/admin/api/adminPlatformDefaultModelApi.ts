/**
 * REST client for the platform default model (#6826) —
 * `/api/v2/admin/gateway/default_model` and the per-model
 * `/platform_models/{id}/default_usage` count.
 *
 * The value is the public project's stored default. Precedence, for one
 * project: its own default, then this platform default, then the first model
 * of its catalogue. A new project starts with this value. See
 * services/elitea-main/internal/application/configurations/platform_default_model.go.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

import { platformModelKeys } from './adminLlmPlatformModelsApi';

const DEFAULT_URL = '/admin/gateway/default_model';
const MODELS_URL = '/admin/gateway/platform_models';

interface PlatformDefaultCandidate {
  readonly name: string;
  readonly display_name: string;
}

export interface PlatformDefaultModel {
  /** The stored model name; empty when none is stored. */
  readonly model_name: string;
  readonly model_project_id: number | null;
  /**
   * False when a stored model is no longer a platform model offered to every
   * project — deleted, disabled or narrowed. The admin must choose again.
   */
  readonly available: boolean;
  /** The platform models offered to every project. */
  readonly candidates: readonly PlatformDefaultCandidate[];
}

export interface PlatformDefaultUsage {
  readonly model_name: string;
  readonly platform_default: boolean;
  /**
   * Projects other than the catalogue project whose own default is this
   * model, as the default or as a low-tier or high-tier default.
   */
  readonly projects: number;
  /** Another row serves the same model, so the delete releases no default. */
  readonly served_by_another_row: boolean;
}

/**
 * Under the platform-model keys on purpose: every model create, edit and delete
 * already invalidates that prefix, and each of them can change which models
 * may be the default and whether the stored one is still available.
 */
const platformDefaultKeys = {
  all: [...platformModelKeys.all, 'default'] as const,
  usage: (id: number) => [...platformModelKeys.all, 'default', 'usage', id] as const,
};

/** Fills what an absent or partial body leaves out. */
export function normalisePlatformDefault(body: Partial<PlatformDefaultModel> | undefined): PlatformDefaultModel {
  return {
    model_name: body?.model_name ?? '',
    model_project_id: body?.model_project_id ?? null,
    available: body?.available ?? true,
    candidates: body?.candidates ?? [],
  };
}

async function readDefault(init?: RequestInit): Promise<PlatformDefaultModel> {
  const body = unwrapBody(await eliteaFetch<unknown>(DEFAULT_URL, init)) as
    | Partial<PlatformDefaultModel>
    | undefined;
  return normalisePlatformDefault(body);
}

/** `GET /admin/gateway/default_model`. */
export function useAdminPlatformDefaultModel(): UseQueryResult<PlatformDefaultModel, Error> {
  return useQuery({ queryKey: platformDefaultKeys.all, queryFn: () => readDefault() });
}

/**
 * `PUT` with a model name, or `DELETE` with `null`. Both answer the stored
 * view, which replaces the cached one.
 */
export function useSavePlatformDefaultModel(): UseMutationResult<PlatformDefaultModel, Error, string | null> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name: string | null) =>
      readDefault(
        name === null
          ? { method: 'DELETE' }
          : {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ model_name: name }),
            },
      ),
    onSuccess: (view) => queryClient.setQueryData(platformDefaultKeys.all, view),
  });
}

/**
 * `GET /admin/gateway/platform_models/{id}/default_usage`, read when a delete
 * is about to be confirmed. `undefined` reads nothing.
 */
export function usePlatformModelDefaultUsage(
  id: number | undefined,
): UseQueryResult<PlatformDefaultUsage, Error> {
  return useQuery({
    queryKey: platformDefaultKeys.usage(id ?? 0),
    enabled: id !== undefined,
    queryFn: async (): Promise<PlatformDefaultUsage> => {
      const body = unwrapBody(
        await eliteaFetch<unknown>(`${MODELS_URL}/${String(id)}/default_usage`),
      ) as Partial<PlatformDefaultUsage> | undefined;
      return {
        model_name: body?.model_name ?? '',
        platform_default: body?.platform_default ?? false,
        projects: body?.projects ?? 0,
        served_by_another_row: body?.served_by_another_row ?? false,
      };
    },
  });
}
