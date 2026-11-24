/**
 * The administration scopes a person may give a new token (ADR-0059 R1.3b):
 * `GET /auth/token/admin_scopes`.
 *
 * The list comes from the SERVER, never from the browser's copy of the
 * caller's permissions: the server intersects the calling credential's own
 * set with the registry's token scopes, so a `token: never` permission is
 * never offered, and it refuses the create anyway. Hiding is not enforcement.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import {
  getGetPersonalTokenAdminScopeOfferQueryKey,
  getPersonalTokenAdminScopeOffer,
} from '@/shared/api/generated/auth/auth';

interface TokenScopeOption {
  readonly name: string;
  readonly area: string;
  readonly class: string;
}

export interface TokenScopeOffer {
  /** False when this sign-in may set no scope (a desktop or mobile session, a token, a break-glass or elevated session). */
  readonly settable: boolean;
  readonly code?: string;
  readonly scopes: readonly TokenScopeOption[];
}

/* manifest: tokens.adminScopeOffer */
async function fetchTokenScopeOffer(): Promise<TokenScopeOffer> {
  const response = await getPersonalTokenAdminScopeOffer();
  return (response as { data: TokenScopeOffer }).data;
}

export function useTokenScopeOfferQuery(): UseQueryResult<TokenScopeOffer, Error> {
  return useQuery({
    queryKey: getGetPersonalTokenAdminScopeOfferQueryKey(),
    queryFn: fetchTokenScopeOffer,
    refetchOnWindowFocus: false,
    // A failed read offers nothing; the create still works without scopes.
    retry: false,
  });
}
