/**
 * REST client for the admin LLM Proxy section's platform MODELS —
 * `/api/v2/admin/gateway/platform_models`.
 *
 * ## The other half of a platform provider
 *
 * A platform credential authenticates to a provider and dispatches nothing on
 * its own. What a caller addresses is a MODEL: a shared row in the public
 * project's schema, in one of the five sections the gateway dispatches, naming
 * the credential it uses.
 *
 * A platform model may name a PLATFORM credential only, and that is the
 * gateway's rule rather than this screen's: it resolves a public model against
 * public credentials alone, deliberately, so "a published model must not resolve
 * differently for each caller". The server refuses a link to anything else.
 *
 * ## `credential_resolves` reports a state nothing else shows
 *
 * A model whose link does not resolve, and a model that names no link at all,
 * are both stored and both undispatchable: admission refuses them and every
 * reader selects on `status_ok`. The listing reports the reason because this
 * screen is the only place an operator can see it.
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

const MODELS_URL = '/admin/gateway/platform_models';

/**
 * Human labels for the five dispatchable model types.
 *
 * The types the server admits arrive as `model_types` on the listing; this maps
 * them to words. A type without an entry renders as its own name rather than
 * disappearing, so a newer server cannot make an option invisible.
 */
export function platformModelTypeLabel(type: string): string {
  const labels: Record<string, string> = {
    llm_model: 'Chat / completion',
    embedding_model: 'Embedding',
    image_generation_model: 'Image generation',
    asr_model: 'Speech to text',
    tts_model: 'Text to speech',
  };
  return labels[type] ?? type;
}

export interface PlatformModel {
  readonly id: number;
  readonly uuid: string;
  /** The id a caller addresses the model by. */
  readonly elitea_title: string;
  readonly type: string;
  readonly section: string;
  /** The gateway dispatches this model only when true. */
  readonly status_ok: boolean;
  readonly status_logs: string;
  /** The provider's own model string. */
  readonly model_name: string;
  /**
   * The platform credential this model uses, by name. Empty means the row names
   * none, which this surface no longer writes and the server no longer accepts.
   */
  readonly credential_name: string;
  /**
   * False when the row names no credential at all, and when the one it names is
   * not among the platform's published providers. Neither row is dispatched.
   */
  readonly credential_resolves: boolean;
  /**
   * The two `data` flags a project's AI configuration filters its tier defaults
   * on. Read back because the edit dialog rewrites `data` whole: a form that
   * could not see them would clear the flag of every model it saved.
   */
  readonly low_tier?: boolean;
  readonly high_tier?: boolean;
  readonly created_at: string;
  readonly updated_at: string;
}

export interface PlatformModelList {
  readonly items: readonly PlatformModel[];
  readonly total: number;
  readonly public_project_id: number;
  /** The model types this deployment dispatches. */
  readonly model_types: readonly string[];
  /** The platform credentials a model may name. */
  readonly credential_names: readonly string[];
  /**
   * Why the credential list could not be read, when it could not. Without it an
   * empty list renders as "this platform has published no providers", which is
   * the reading that sends an operator to create a duplicate — and every model
   * would show as unresolved for the same reason.
   */
  readonly credential_error?: string;
}

/**
 * What the model dialog sends. `section` is derived by the server.
 *
 * `ai_credentials` is REQUIRED, and the registry is where that comes from: all
 * five model types declare it, so a body without it fails the create's
 * required-field rule and — were it stored — would fail provider admission and
 * be served by nobody.
 *
 * The tier flags are optional because only `llm_model` declares them. The other
 * four decode their `data` with unknown fields refused, so sending a flag they
 * do not declare makes the row an invalid binding.
 */
export interface PlatformModelDraft {
  readonly elitea_title: string;
  readonly type: string;
  readonly data: {
    readonly name: string;
    readonly ai_credentials: { readonly elitea_title: string };
    readonly low_tier?: boolean;
    readonly high_tier?: boolean;
  };
}

/**
 * Exported because a PROVIDER mutation has to invalidate this listing too.
 *
 * The listing carries `credential_names` — the platform providers a model may
 * name — so it is the query that backs the model dialog's "Platform provider"
 * select. Creating a provider invalidated only the providers query, and this
 * one stayed cached: the operator added a provider, opened "Add a platform
 * model", and the select was still empty until a full reload, although the
 * server was already returning the new name.
 *
 * `adminLlmProvidersApi.ts` is the only importer, and it imports nothing else
 * from this module, so the two files do not form a cycle.
 */
export const platformModelKeys = {
  all: ['admin', 'llmProxy', 'platformModels'] as const,
};

/**
 * Fills in what an absent or partial body leaves out.
 *
 * `credential_error` is carried through when present and OMITTED when not — the
 * distinction is the contract: an empty credential list with no error is a
 * platform that has published no providers, and one WITH an error is a list
 * that could not be read. The second would otherwise render as the first.
 */
function normalisePlatformModels(body: PlatformModelList | undefined): PlatformModelList {
  // Early return for the absent body, so the field list below is not five
  // optional chains deep — the same split `normaliseCatalogue` makes.
  if (body === undefined) {
    return {
      items: [],
      total: 0,
      public_project_id: 0,
      model_types: [],
      credential_names: [],
    };
  }
  const base = {
    items: body.items ?? [],
    total: body.total ?? 0,
    public_project_id: body.public_project_id ?? 0,
    model_types: body.model_types ?? [],
    credential_names: body.credential_names ?? [],
  };
  if (body.credential_error === undefined) return base;
  return { ...base, credential_error: body.credential_error };
}

/** `GET /admin/gateway/platform_models`. */
export function useAdminPlatformModels(): UseQueryResult<PlatformModelList, Error> {
  return useQuery({
    queryKey: platformModelKeys.all,
    queryFn: async (): Promise<PlatformModelList> => {
      const body = unwrapBody(await eliteaFetch<unknown>(MODELS_URL)) as
        | PlatformModelList
        | undefined;
      return normalisePlatformModels(body);
    },
  });
}

export function useCreatePlatformModel(): UseMutationResult<void, Error, PlatformModelDraft> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: PlatformModelDraft) => {
      await eliteaFetch<unknown>(MODELS_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: platformModelKeys.all }),
  });
}

/**
 * `PUT /admin/gateway/platform_models/{id}` — a PARTIAL update.
 *
 * The TYPE is not resent: it cannot change, because the server derives the
 * `section` from it and a stored row's pair is already correct. Restating it
 * would make a partial update carry a field the operator did not touch.
 */
export function useUpdatePlatformModel(): UseMutationResult<
  void,
  Error,
  { readonly id: number; readonly draft: Omit<PlatformModelDraft, 'type'> }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, draft }) => {
      await eliteaFetch<unknown>(`${MODELS_URL}/${String(id)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: platformModelKeys.all }),
  });
}

/**
 * `DELETE /admin/gateway/platform_models/{id}`.
 *
 * Withdraws the model from every project at once. Unlike a credential, nothing
 * else references it — a project that had been addressing it simply stops
 * finding it, which surfaces as `model_not_found` on the next call.
 */
export function useDeletePlatformModel(): UseMutationResult<void, Error, number> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: number) => {
      await eliteaFetch<unknown>(`${MODELS_URL}/${String(id)}`, { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: platformModelKeys.all }),
  });
}
