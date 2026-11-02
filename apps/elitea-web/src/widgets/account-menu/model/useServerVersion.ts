/**
 * The server's version (`major.minor`) from the public discovery document,
 * `GET /.well-known/elitea-client` (ADR-0025 decision 1). Asked only once the
 * account menu has opened, in the background; a failure leaves it out.
 */
import { useQuery } from '@tanstack/react-query';

import { getGetClientDiscoveryQueryKey, getGetClientDiscoveryUrl } from '@/shared/api/generated/discovery/discovery';
import { eliteaFetch } from '@/shared/api/generated/mutator';

const ONE_HOUR = 60 * 60_000;

function serverVersionOf(body: unknown): string | undefined {
  if (typeof body !== 'object' || body === null) return undefined;
  const version = (body as { server_version?: unknown }).server_version;
  return typeof version === 'string' && version !== '' ? version : undefined;
}

export function useServerVersion(enabled: boolean): string | undefined {
  const query = useQuery({
    queryKey: getGetClientDiscoveryQueryKey(),
    // The document is root-mounted, beside /api/v2 (`originRoot`).
    queryFn: ({ signal }) =>
      eliteaFetch<{ data: unknown }>(getGetClientDiscoveryUrl(), { method: 'GET', signal }, { background: true, originRoot: true }),
    enabled,
    staleTime: ONE_HOUR,
    retry: false,
  });
  return serverVersionOf(query.data?.data);
}
