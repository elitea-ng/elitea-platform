/**
 * REST client for the admin LLM Proxy section's **Providers** tab —
 * `/api/v2/admin/gateway/providers`.
 *
 * ## What a "global provider" is
 *
 * A platform-wide provider credential is a row in the PUBLIC project's schema
 * with `shared = true`. The LLM gateway already resolves that scope for every
 * request — the caller's own project, plus the public project's shared rows
 * (issue #316) — so a credential published here is usable by every project on
 * the platform without any project configuring it.
 *
 * That is the same mechanism, not a new one. Nothing about the credential model
 * changed to make this screen possible; what was missing was a surface, because
 * publishing one previously meant knowing that "project 1, ticked shared" is the
 * global scope and being a member of that project.
 *
 * ## The secret is never on the wire, in either direction, more than once
 *
 * A read NEVER returns secret material — not the value, and not even the
 * `{{secret.NAME}}` reference. What comes back is which secret fields are SET
 * and whether each is SEALED. So there is nothing to pre-fill an edit form with,
 * and leaving a secret field untouched on an edit sends no secret at all and
 * keeps the stored one — the same contract `./adminMcpServersApi` keeps for a
 * client secret, and for the same reason.
 *
 * A write sends the plaintext once. The server seals it into the public
 * project's vault inside the same transaction that writes the row, so the row
 * holds a reference and never the key.
 *
 * ## `status_ok` is the field that decides whether anything happens
 *
 * The gateway admits `status_ok = true` and nothing else. The server runs
 * provider admission on every write, so a credential that does not resolve is
 * stored, listed, and completely inert. That state has no other display anywhere,
 * which is why it is a column on this table rather than a detail.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes.
 *
 * ## The two operations on a SAVED row: `check` and `revalidate`
 *
 * `POST /{id}/check` dials the real provider through the same stored-check
 * path a project's own credentials use (`elitea-main`'s `stored_check.go`,
 * reached here via `global_providers.go`'s `CheckGlobalProviderConnection`)
 * and writes NOTHING. `POST /{id}/revalidate` re-runs ADMISSION — do the
 * row's references still expand, do its secrets still redeem — and persists
 * only `status_ok`; it never dials a provider. The server keeps the two
 * separate so a provider outage cannot withdraw every platform credential at
 * once, and this client mirrors that: `useCheckAdminLlmProvider` never
 * touches the query cache, `useRevalidateAdminLlmProvider` never contacts a
 * provider.
 *
 * `check`'s contract is the ONE place this file has to reach past
 * `eliteaFetch`'s throw-on-non-2xx contract: the route answers
 * `{"success":false,"message":...}` on ITS OWN 400 ("could not verify") and
 * 404 ("configuration not found") — that is the real, renderable answer, not
 * a transport failure — but both are non-2xx on the wire, so `eliteaFetch`
 * throws for them regardless (`mutator.ts`'s own contract, shared by every
 * hook here). `useCheckAdminLlmProvider` catches exactly that documented
 * shape and resolves with it, the same duck-typed catch
 * `pages/credentials/useCredentialConnectionTest.ts`'s `performStoredTest`
 * uses for the project-scoped twin of this route. Anything else — a network
 * failure, an auth redirect, a body this build does not recognise — is left
 * to throw, so the mutation's `isError` still fires for those.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

const PROVIDERS_URL = '/admin/gateway/providers';

/**
 * Every credential type this app knows how to draw a form for.
 *
 * IT IS NOT THE LIST THE SELECT OFFERS. What a deployment will actually publish
 * comes from the server, as `provider_types` on the listing. A type outside the
 * server's own catalogue cannot be given a `section` or have its key sealed, so
 * the server refuses it. The shipped catalogue describes all nine today; a
 * deployment's may describe fewer, and the select must follow the server either
 * way. This list exists so the form has field definitions ready for whatever
 * the server offers — see `../llmProviderForm.ts`.
 *
 * The server's refusal is the security boundary, never this list.
 */
export const LLM_PROVIDER_TYPES = [
  'open_ai',
  'azure_open_ai',
  'open_ai_azure',
  'ai_dial',
  'anthropic',
  'ollama',
  'amazon_bedrock',
  'vertex_ai',
  'vllm',
] as const;

export type LlmProviderType = (typeof LLM_PROVIDER_TYPES)[number];

/**
 * One secret field's status. The value itself is never sent.
 *
 * Not exported: it is only ever reached through `LlmProvider.secrets`, and an
 * export with no importer is what the dead-code gate is for.
 */
interface LlmProviderSecret {
  readonly field: string;
  readonly set: boolean;
  /**
   * False when the row holds the value literally rather than as a vault
   * reference — a finding, because such a row is readable by every holder of
   * the project-scoped configuration permissions on the public project.
   */
  readonly sealed: boolean;
}

export interface LlmProvider {
  readonly id: number;
  readonly uuid: string;
  readonly elitea_title: string;
  readonly label: string;
  readonly type: string;
  /** The gateway admits this credential only when true. */
  readonly status_ok: boolean;
  readonly status_logs: string;
  /** `api_base` — not a secret. */
  readonly endpoint: string;
  /** The remaining non-secret fields, by provider. */
  readonly settings: Readonly<Record<string, string>>;
  readonly secrets: readonly LlmProviderSecret[];
  readonly created_at: string;
  readonly updated_at: string;
}

export interface LlmProviderList {
  readonly items: readonly LlmProvider[];
  readonly total: number;
  /**
   * The credential types THIS deployment will publish, from its own catalogue.
   * Read rather than assumed: a hardcoded client list drifts from the registry
   * snapshot, and the drift shows up as a refusal on save rather than as an
   * absent option.
   */
  readonly provider_types: readonly string[];
  /**
   * Which project this deployment treats as the shared one. Echoed so an
   * operator can confirm it: getting it wrong is the failure where every
   * credential is published correctly into a schema the gateway never reads.
   */
  readonly public_project_id: number;
}

/**
 * What the provider dialog sends.
 *
 * `data` carries the provider fields — `api_base`, `api_key` and the
 * per-provider extras. A secret key is present ONLY when the operator entered
 * one: on an edit, an absent key means "keep what is stored", and sending an
 * empty string instead would clear a working credential.
 */
export interface LlmProviderDraft {
  readonly elitea_title: string;
  readonly type: LlmProviderType;
  readonly data: Readonly<Record<string, string | boolean>>;
}

const providerKeys = {
  all: ['admin', 'llmProxy', 'providers'] as const,
};

/** `GET /admin/gateway/providers`. */
export function useAdminLlmProviders(): UseQueryResult<LlmProviderList, Error> {
  return useQuery({
    queryKey: providerKeys.all,
    queryFn: async (): Promise<LlmProviderList> => {
      const body = unwrapBody(await eliteaFetch<unknown>(PROVIDERS_URL)) as
        | LlmProviderList
        | undefined;
      return {
        items: body?.items ?? [],
        total: body?.total ?? 0,
        public_project_id: body?.public_project_id ?? 0,
        provider_types: body?.provider_types ?? [],
      };
    },
  });
}

/**
 * `POST /admin/gateway/providers`.
 *
 * `shared` is NOT sent. The server forces it — and refuses an explicit `false`
 * rather than overriding it — so a client that sent the field would either be
 * restating the server's rule or be refused for contradicting it.
 */
export function useCreateAdminLlmProvider(): UseMutationResult<void, Error, LlmProviderDraft> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: LlmProviderDraft) => {
      await eliteaFetch<unknown>(PROVIDERS_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: providerKeys.all }),
  });
}

/**
 * `PUT /admin/gateway/providers/{id}` — a PARTIAL update.
 *
 * Only the fields the body carries are written. That is the delegated handler's
 * contract and it is load-bearing here: sending a whole `data` object on an
 * edit whose secret field the operator left blank would erase the stored key.
 */
export function useUpdateAdminLlmProvider(): UseMutationResult<
  void,
  Error,
  { readonly id: number; readonly draft: Partial<LlmProviderDraft> }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, draft }) => {
      await eliteaFetch<unknown>(`${PROVIDERS_URL}/${String(id)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: providerKeys.all }),
  });
}

/**
 * `DELETE /admin/gateway/providers/{id}`.
 *
 * Deleting a platform credential withdraws it from EVERY project at once,
 * including projects whose models name it. There is no cheap way to count those
 * — they live in one schema per project — so the dialog says so rather than
 * reporting a number it would have to invent.
 */
export function useDeleteAdminLlmProvider(): UseMutationResult<void, Error, number> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: number) => {
      await eliteaFetch<unknown>(`${PROVIDERS_URL}/${String(id)}`, { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: providerKeys.all }),
  });
}

/**
 * One live-check verdict — `POST /{id}/check`'s response, byte for byte
 * (`success` always present; `message` and `unsupported` only when the row's
 * own answer carried them). This is NOT `LlmProvider.status_ok`: that column
 * is the ADMISSION decision (do this row's references expand, do its secrets
 * redeem), computed at write time and re-derived only by revalidate. This is
 * a real provider round trip, run on demand, and the two can disagree in
 * either direction — see `LlmProxyProvidersPanel.tsx` for why both are shown.
 */
export interface LlmProviderCheckResult {
  readonly success: boolean;
  readonly message?: string;
  /** The row's type has no working checker in this build — not a failed round trip. */
  readonly unsupported?: boolean;
}

/** `body` narrowed to `LlmProviderCheckResult`, or `undefined` for a shape that is not this route's documented contract (neither the 200 nor the 400/404 form carries anything less than a `success` boolean). */
function asProviderCheckResult(body: unknown): LlmProviderCheckResult | undefined {
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as Record<string, unknown>;
  if (typeof record['success'] !== 'boolean') return undefined;
  const message = record['message'];
  return {
    success: record['success'],
    ...(typeof message === 'string' && message !== '' ? { message } : {}),
    ...(record['unsupported'] === true ? { unsupported: true } : {}),
  };
}

/**
 * `POST /admin/gateway/providers/{id}/check` — a real provider round trip for
 * a row that is already saved, with no request body: the secret is sealed in
 * the public project's vault, so this screen has no copy of it to resend, and
 * the server reads and redeems the row itself.
 *
 * Resolves — never throws — for the route's own documented failures (the
 * module header explains why `eliteaFetch`'s throw has to be caught here). A
 * transport-level failure this route did not itself answer still rejects, so
 * `isError` is exactly the signal for "the check could not be run" as
 * distinct from "the check ran and failed".
 */
export function useCheckAdminLlmProvider(): UseMutationResult<LlmProviderCheckResult, Error, number> {
  return useMutation({
    mutationFn: async (id: number): Promise<LlmProviderCheckResult> => {
      try {
        const body = unwrapBody(
          await eliteaFetch<unknown>(`${PROVIDERS_URL}/${String(id)}/check`, { method: 'POST' }),
        );
        const result = asProviderCheckResult(body);
        if (result !== undefined) return result;
      } catch (error) {
        if (error instanceof EliteaApiError && error.failure.kind === 'http') {
          const result = asProviderCheckResult(error.failure.body);
          if (result !== undefined) return result;
        }
        throw error;
      }
      throw new Error(
        `admin llm provider check: unrecognised response shape for configuration ${String(id)}`,
      );
    },
  });
}

/** The fields of `POST /{id}/revalidate`'s response this panel reads — see `useRevalidateAdminLlmProvider` for why the rest of that object is not typed here. */
export interface LlmProviderRevalidateResult {
  readonly status_ok: boolean;
  readonly status_logs: string;
}

function asRevalidateResult(body: unknown): LlmProviderRevalidateResult {
  const record = typeof body === 'object' && body !== null ? (body as Record<string, unknown>) : {};
  return {
    status_ok: record['status_ok'] === true,
    status_logs: typeof record['status_logs'] === 'string' ? record['status_logs'] : '',
  };
}

/**
 * `POST /admin/gateway/providers/{id}/revalidate` — re-runs admission for a
 * saved row and persists `status_ok`. No provider is contacted.
 *
 * The response is the full `Configuration` row the delegated Go handler
 * (`revalidate.go`) answers with — `{id, name, type, section, data,
 * status_ok, ...}` — NOT the `LlmProvider` shape `useAdminLlmProviders` lists
 * (`elitea_title`/`endpoint`/`settings`/`secrets`). The two disagree on field
 * names (`name` vs `elitea_title`) and the listing's shape is a
 * SERVER-SIDE REDACTION of `data` that this route's response never applies,
 * so decoding the whole object into a row would either drop fields silently
 * or need a second redaction layer here for a response this panel reads
 * exactly one real answer from. `status_ok` (and `status_logs`, the
 * admission decision's own account of why) are read, and the query cache is
 * PATCHED rather than replaced with the response — everything else about the
 * row (its endpoint, its settings, its secrets) came from the listing, and
 * revalidation does not touch any of it.
 */
export function useRevalidateAdminLlmProvider(): UseMutationResult<
  LlmProviderRevalidateResult,
  Error,
  number
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: number): Promise<LlmProviderRevalidateResult> =>
      asRevalidateResult(
        unwrapBody(await eliteaFetch<unknown>(`${PROVIDERS_URL}/${String(id)}/revalidate`, { method: 'POST' })),
      ),
    onSuccess: (result, id) => {
      // A patch, not an invalidation: the listing's OWN read redacts `data`
      // into `endpoint`/`settings`/`secrets`, which this response does not
      // carry at all, so refetching is the only way to get those back — this
      // way the row keeps them and only the two admission fields move.
      queryClient.setQueryData<LlmProviderList>(providerKeys.all, (current) => {
        if (current === undefined) return current;
        return {
          ...current,
          items: current.items.map((item) =>
            item.id === id
              ? { ...item, status_ok: result.status_ok, status_logs: result.status_logs }
              : item,
          ),
        };
      });
    },
  });
}
