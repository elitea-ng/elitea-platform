/**
 * REST client for the GLOBAL secret vault — the admin Secrets surface (unit A14).
 *
 * ## This is NOT the project vault, and the two must not be confused
 *
 * `entities/secret/api/secretApi.ts` addresses `…/secrets/{secrets,secret}/default/{projectID}`:
 * ONE project's vault, row `project-<id>` in `centry.secrets_data`. This module
 * addresses the `administration` mode of the same routes, which the legacy
 * runtime maps to a different handler over a different row — `admin` — whose
 * contents are merged into EVERY project's `{{secret.…}}` resolution
 * (`EngineBase.get_all_secrets` folds in the global vault as `shared_secrets`).
 *
 * They therefore share no endpoint, no body shape and no query key:
 *
 * | | project (`default`) | global (`administration`) |
 * |---|---|---|
 * | list | `[{name, secret_name, is_default}]` | `[{name, secret: "******"}]` |
 * | read one | `{name, secret_name, value, is_hidden}` | `{secret: "<value>"｜null}` |
 * | create | `POST …/secrets/default/{id}` `{name, value}` | `POST …/secret/administration/0/{name}` `{secret}` |
 * | update | `PUT …/secret/default/{id}/{name}` `{name, value}` | `PUT …/secret/administration/0/{name}` `{secret:{old_name, value}}` |
 *
 * The `0` in every administration path is a PLACEHOLDER. The legacy admin
 * handlers take a `project_id` parameter and ignore it entirely, and admin_ui
 * has always sent `0`; `services/elitea-main/internal/api/v2/secrets/admin.go`
 * ignores it the same way. It is kept because the route shape is the legacy one
 * and inventing a different one is what #137 cost.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminUsersApi`.
 *
 * ## Values never enter the query cache
 *
 * `useRevealAdminSecret` is a MUTATION, not a query, even though it is a GET.
 * A query would park plaintext credentials in the TanStack cache, keyed and
 * retained, where every devtools panel and every later `getQueryData` can reach
 * them. The reveal is a one-shot fetch whose result the caller holds for as long
 * as the operator keeps the row open and no longer.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapListPage } from '@/shared/api/unwrap';

/** The legacy mode that selects the global-vault handler. */
const ADMIN_MODE = 'administration';

/**
 * The ignored `project_id` segment. See this module's header — it is the legacy
 * route shape, not a project reference, and the server never keys on it.
 */
const IGNORED_PROJECT_SEGMENT = '0';

const LIST_URL = `/secrets/secrets/${ADMIN_MODE}/${IGNORED_PROJECT_SEGMENT}`;

function secretUrl(name: string): string {
  return `/secrets/secret/${ADMIN_MODE}/${IGNORED_PROJECT_SEGMENT}/${encodeURIComponent(name)}`;
}

/**
 * One row of the listing. `secret` is always the literal mask `******` — the
 * listing never carries plaintext, so this field is not the value and is not
 * exposed by the hook.
 */
interface AdminSecretListRow {
  readonly name: string;
  readonly secret: string;
}

/** What the page works with: a name, and nothing else until the operator asks. */
export interface AdminSecret {
  readonly name: string;
}

/**
 * One query-key namespace, declared once.
 *
 * All three mutations invalidate `adminSecretsKeys.all`, so a key built ad hoc
 * at a call site would be a cache the writes never refresh — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const adminSecretsKeys = {
  all: ['admin', 'secrets'] as const,
  list: () => ['admin', 'secrets', 'list'] as const,
};

/**
 * The server's own explanation of a refusal, when it gave one.
 *
 * The refusals this surface produces are specific and actionable, and a generic
 * "Failed to…" would throw all of them away:
 *
 *   - `400 {"message":"Secret \"x\" already exists"}` — the create the page's own
 *     duplicate check did not catch, because the listing was stale.
 *   - `400 {"message":"secret name must contain only letters, digits and
 *     underscores"}` — the server's copy of the name rule.
 *   - `404 {"message":"Project secret was not found"}` — an edit of a secret
 *     something else deleted in between.
 *   - `500 {"message":"global vault is unreadable"}` — the vault rows exist and
 *     cannot be opened, which is a deployment fault (a wrong `SECRETS_MASTER_KEY`
 *     is the usual cause) and NOT an empty vault. The handler distinguishes the
 *     two deliberately: reporting "no secrets" here would invite a write that
 *     replaced the vault.
 *
 * A 403 IS among them now. `shared/api/http.ts` used to escalate 401 AND 403
 * into the re-auth flow, whose `failure.kind === 'auth'` branch carries no body,
 * so a caller refused for lack of `configuration.secrets.secret.view` — a real
 * state, since the administration-mode `editor` role holds every write
 * permission on secrets and not that one — got the generic message rather than
 * a sentence naming the permission. Issue 93 changed that shared 403 policy:
 * an authorization refusal is not a session failure, arrives as `kind: 'http'`,
 * and explains itself here like any other 4xx.
 */
export function adminSecretFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  // Only `http` can explain itself; see this function's header for what that
  // excludes and why.
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as { message?: unknown; error?: unknown };
  const reason = typeof record.message === 'string' ? record.message : record.error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** `GET /secrets/secrets/administration/0` — every global secret NAME, masked. */
export function useAdminSecrets(): UseQueryResult<AdminSecret[], Error> {
  return useQuery({
    queryKey: adminSecretsKeys.list(),
    queryFn: async (): Promise<AdminSecret[]> =>
      unwrapListPage<AdminSecretListRow>(await eliteaFetch<unknown>(LIST_URL), 'adminSecrets').rows.map(
        (row) => ({ name: row.name }),
      ),
  });
}

/**
 * `GET /secrets/secret/administration/0/{name}` — one plaintext value.
 *
 * A mutation on purpose; see this module's header. `null` is the server's answer
 * for a name it does not hold, and is passed through rather than coerced to `''`
 * so the caller can tell "empty secret" from "no such secret".
 */
export function useRevealAdminSecret(): UseMutationResult<string | null, Error, string> {
  return useMutation({
    mutationFn: async (name: string): Promise<string | null> => {
      // `unwrapBody` (R-A6) rather than a hand-rolled `.data.secret`: the
      // envelope is `eliteaFetch`'s, and #132 is what reaching into it by hand
      // at a call site costs.
      const body = unwrapBody(await eliteaFetch<unknown>(secretUrl(name)));
      if (typeof body !== 'object' || body === null) return null;
      const value = (body as { secret?: unknown }).secret;
      return typeof value === 'string' ? value : null;
    },
  });
}

export interface AdminSecretWrite {
  readonly name: string;
  readonly value: string;
}

/** `POST /secrets/secret/administration/0/{name}` with `{"secret": "<value>"}`. */
export function useCreateAdminSecret(): UseMutationResult<void, Error, AdminSecretWrite> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ name, value }: AdminSecretWrite) => {
      await eliteaFetch<unknown>(secretUrl(name), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ secret: value }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminSecretsKeys.all }),
  });
}

/**
 * `PUT /secrets/secret/administration/0/{name}` with the NESTED
 * `{"secret":{"old_name","value"}}` body the legacy API defined.
 *
 * `old_name` names the entry being replaced and the URL name is what it becomes,
 * so this one call expresses both a value change (`old_name === name`) and a
 * rename. The page only ever sends the first, matching the reference — its Edit
 * dialog renders the name field disabled — but the parameter is explicit rather
 * than implied so a future rename affordance cannot be built on a guess.
 */
export function useUpdateAdminSecret(): UseMutationResult<void, Error, AdminSecretWrite> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ name, value }: AdminSecretWrite) => {
      await eliteaFetch<unknown>(secretUrl(name), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ secret: { old_name: name, value } }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminSecretsKeys.all }),
  });
}

/** `DELETE /secrets/secret/administration/0/{name}` — 204, and the name is gone. */
export function useDeleteAdminSecret(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (name: string) => {
      await eliteaFetch<unknown>(secretUrl(name), { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminSecretsKeys.all }),
  });
}
