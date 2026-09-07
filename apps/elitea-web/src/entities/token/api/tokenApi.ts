/**
 * Hand-written REST client for the personal access tokens domain (settings
 * section — PERSONAL tokens tab).
 *
 * Source: `apps/elitea-ui/src/api/auth.js` — RTK Query endpoints
 * (`tokenList` / `tokenCreate` / `tokenDelete`). Every route below maps to a
 * real, wired Go handler (`services/elitea-main/internal/api/v2/auth/
 * handler.go:30-32`):
 *
 *   - GET  `/auth/token/`             → list
 *   - POST `/auth/token/`             → create
 *   - DELETE `/auth/token/{tokenUUID}` → delete
 *
 * ## Generated transport, hand-written cache policy (issue 36, item 6)
 *
 * The four routes are described in `services/elitea-main/api/openapi/v2.yaml`,
 * so the URLs and the response types come from `shared/api/generated/auth` and
 * are no longer re-derived here. What stays hand-written is the part orval does
 * not generate usefully: it shapes EVERY operation as a `useQuery` gated by
 * `enabled`, including the create and the delete, so the mutations below wrap
 * the generated FETCHERS and own the invalidation.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import {
  createPersonalToken as createPersonalTokenRequest,
  deletePersonalToken as deletePersonalTokenRequest,
  listPersonalTokens as listPersonalTokensRequest,
} from '@/shared/api/generated/auth/auth';

import type { PersonalAccessToken, TokenExpirationRequest } from '../model/types';

/*
 * `eliteaFetch` — and so every generated fetcher built on it — resolves the
 * `{data, status, headers}` envelope, NEVER the bare body. Reading the result
 * as if it were the token is how `GeneratedTokenDialog` once rendered an EMPTY
 * name and an EMPTY secret for a key that is only ever shown once (#132).
 */

/* ── query key ─────────────────────────────────────────────────────────── */

function tokensQueryKey(): string[] {
  return ['settings', 'personal', 'tokens'];
}

/* ── tokenList — GET /auth/token/ ──────────────────────────────────────── */
/* manifest: tokens.list */

export async function listTokens(): Promise<readonly PersonalAccessToken[]> {
  const response = await listPersonalTokensRequest();
  return (response as { data: readonly PersonalAccessToken[] }).data;
}

export function useListTokensQuery(options: { enabled?: boolean } = {}): UseQueryResult<readonly PersonalAccessToken[], Error> {
  return useQuery({
    queryKey: tokensQueryKey(),
    queryFn: listTokens,
    enabled: options.enabled ?? true,
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}

/* ── tokenCreate — POST /auth/token/ ───────────────────────────────────── */
/* manifest: tokens.create */

export interface CreateTokenParams {
  readonly name: string;
  /** `null` for never-expiring, or `{ measure, value }` for time-bound. */
  readonly expires: TokenExpirationRequest;
  /**
   * OPTIONAL creation-time project binding (`spec-llm-project-scope` §4).
   *
   * OMIT the field for an unbound token. Unbound is the existing behaviour
   * and the spec makes it the mandatory default, so this field must never
   * acquire a non-`undefined` default: `createToken` serializes `params`
   * directly, and an absent key is an absent `project_id` in the body.
   *
   * There is no update path. The server fixes the binding at creation, so a
   * caller cannot change it later.
   */
  readonly project_id?: number;
}

export interface CreatedTokenResponse {
  readonly uuid: string;
  readonly name: string;
  readonly token: string;
  readonly expires: string | null;
  /** The bound project, or `null`/absent when the token is unbound. */
  readonly project_id?: number | null;
}

/*
 * NOTE: the `.data` step is not optional. The generated fetcher resolves the
 * `{data, status, headers}` envelope, and reading the result as the token gave
 * `resp.token` / `resp.name` of `undefined`: the freshly minted PAT was never
 * shown to the user, and it is only ever shown once. Caught by
 * e2e/journeys/settings/settings.tokens.spec.ts.
 */
export async function createToken(params: CreateTokenParams): Promise<CreatedTokenResponse> {
  const response = await createPersonalTokenRequest({ ...params });
  return (response as { data: CreatedTokenResponse }).data;
}

export function useCreateTokenMutation(): UseMutationResult<CreatedTokenResponse, Error, CreateTokenParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: createToken,
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: tokensQueryKey() }),
  });
}

/* ── tokenDelete — DELETE /auth/token/{tokenUUID} ──────────────────────── */
/* manifest: tokens.delete */

export async function deleteToken(tokenUUID: string): Promise<void> {
  // ENCODED HERE, not by the generated URL builder: orval interpolates a path
  // parameter verbatim.
  await deletePersonalTokenRequest(encodeURIComponent(tokenUUID));
}

export function useDeleteTokenMutation(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (uuid: string) => deleteToken(uuid),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: tokensQueryKey() }),
  });
}
