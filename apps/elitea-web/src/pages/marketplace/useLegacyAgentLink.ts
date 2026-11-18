/**
 * Finishes an old `?agentId=` link the redirect could not resolve (the project
 * was not selected yet, or the lookup failed). Once a project is known it asks
 * again, retrying a failed lookup; a found listing opens its page, and "no
 * listing" lands on the Marketplace home. The result tells the page whether to
 * show a retry.
 */
import { useEffect } from 'react';

import { useQuery } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';

import { findListingIdByEntity } from './legacyLink';

export interface LegacyAgentLink {
  /** The lookup failed after its retries. */
  readonly failed: boolean;
  readonly retry: () => void;
}

export function useLegacyAgentLink(projectId: string | undefined, agentId: string | undefined): LegacyAgentLink {
  const navigate = useNavigate();
  const active = agentId !== undefined && agentId !== '';
  const query = useQuery({
    queryKey: ['marketplace', 'legacy-agent-link', projectId, agentId],
    enabled: active && projectId !== undefined,
    queryFn: () => findListingIdByEntity(projectId ?? '', agentId ?? ''),
    retry: 3,
    retryDelay: (attempt) => Math.min(500 * 2 ** attempt, 4000),
  });
  useEffect(() => {
    if (!active || query.data === undefined) return;
    if (query.data === null) void navigate({ to: '/marketplace', search: {}, replace: true });
    else
      void navigate({
        to: '/marketplace/$kind/$listingId',
        params: { kind: 'agents', listingId: query.data },
        replace: true,
      });
  }, [active, query.data, navigate]);
  return { failed: active && query.isError, retry: () => void query.refetch() };
}
