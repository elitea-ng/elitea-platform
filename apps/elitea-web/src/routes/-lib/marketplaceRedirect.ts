/**
 * Where the old Catalog and Agent HUB addresses go (ADR-0051 decision 1).
 *
 * `/elitea-catalog` and `/agents-hub` are redirect sources only. They keep what
 * their links carried: `?agentId=` (the shared deep link) opens that agent's
 * listing page, and `?tab=skills` selects the skills kind. `agentId` always names an agent, whatever the tab. A link with neither
 * lands on the Marketplace's default, the agents.
 */
import { redirect } from '@tanstack/react-router';

import { kindFromSearch } from '@/pages/marketplace';

export interface LegacyCatalogSearch {
  readonly tab?: string | undefined;
  readonly agentId?: string | undefined;
}

export function redirectToMarketplace(search: LegacyCatalogSearch): never {
  const kind = kindFromSearch(search.tab);
  if (search.agentId !== undefined && search.agentId !== '') {
    // oxlint-disable-next-line typescript/only-throw-error -- TanStack Router's beforeLoad redirect contract: throw the Response redirect() returns, not an Error.
    throw redirect({ to: '/marketplace/$kind/$listingId', params: { kind: 'agents', listingId: search.agentId }, replace: true });
  }
  // oxlint-disable-next-line typescript/only-throw-error -- same contract as above.
  throw redirect({ to: '/marketplace', search: kind === 'skills' ? { kind } : {}, replace: true });
}
