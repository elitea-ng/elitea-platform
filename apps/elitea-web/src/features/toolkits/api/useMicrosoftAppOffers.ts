/**
 * The Default apps a credential form may offer for one credential type in one project
 * (ADR-0057 decision 5; epic 2254 story S1b).
 *
 * `GET /microsoft_oauth_apps/{projectId}?credential_type=` answers the active apps that
 * serve the type and the project, and whether Custom app may be offered next to them.
 * It carries no client ID, authority or secret: a person learns which apps they may
 * pick, never how they are configured. An empty registry answers an empty list, so the
 * form offers only Custom app. The server applies the availability rule again at every
 * sign-in; this read is a convenience, not an authorization.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { listMicrosoftAppOffers } from '@/shared/api/generated/configurations/configurations';
import type { MicrosoftAppOffers } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

export interface UseMicrosoftAppOffersParams {
  /** The credential type, for example `outlook`. `undefined` disables the read. */
  readonly credentialType: string | undefined;
  readonly projectId: string | number | undefined;
}

function numericProjectId(projectId: string | number | undefined): number | undefined {
  const id = typeof projectId === 'string' ? Number(projectId) : projectId;
  return id !== undefined && Number.isInteger(id) && id > 0 ? id : undefined;
}

export function useMicrosoftAppOffers({
  credentialType,
  projectId,
}: UseMicrosoftAppOffersParams): UseQueryResult<MicrosoftAppOffers, Error> {
  const id = numericProjectId(projectId);
  return useQuery({
    queryKey: ['microsoft-app-offers', id, credentialType] as const,
    enabled: id !== undefined && credentialType !== undefined && credentialType !== '',
    retry: false,
    staleTime: 30_000,
    queryFn: async ({ signal }) =>
      unwrapBody(await listMicrosoftAppOffers(id ?? 0, { credential_type: credentialType ?? '' }, { signal })) as MicrosoftAppOffers,
  });
}
