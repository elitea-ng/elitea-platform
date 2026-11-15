/**
 * Where the old Catalog and Agent HUB addresses go (ADR-0051 decision 1).
 *
 * `/elitea-catalog` and `/agents-hub` are redirect sources only. They keep what
 * their links carried: `?agentId=` (the shared deep link) opens that agent's
 * listing page, and `?tab=skills` selects the skills kind. `agentId` always
 * names an agent, whatever the tab. A link with neither lands on the
 * Marketplace's default, the agents.
 *
 * `agentId` is the PUBLIC agent's id, and a listing page is keyed by the
 * LISTING id, so the redirect asks the search route for the listing of that
 * entity (`?entity_id=`). An agent with no listing, or a search that fails,
 * lands on the Marketplace home instead of a dead page.
 */
import { redirect } from '@tanstack/react-router';

import { kindFromSearch } from '@/pages/marketplace';
import { selectProjectId } from '@/pages/skills/lib/useSelectedProjectId';
import { searchMarketplaceListings } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceListingPage } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

export interface LegacyCatalogSearch {
  readonly tab?: string | undefined;
  readonly agentId?: string | undefined;
}

async function listingIdOf(projectId: string | undefined, entityId: string): Promise<string | undefined> {
  if (projectId === undefined) return undefined;
  try {
    const response = await searchMarketplaceListings(projectId, { entity_id: entityId, kind: 'agent', limit: 1 });
    return (unwrapBody(response) as MarketplaceListingPage).rows[0]?.id;
  } catch {
    return undefined;
  }
}

export async function redirectToMarketplace(search: LegacyCatalogSearch, context: unknown): Promise<never> {
  const kind = kindFromSearch(search.tab);
  if (search.agentId !== undefined && search.agentId !== '') {
    const listingId = await listingIdOf(selectProjectId(context), search.agentId);
    if (listingId !== undefined) {
      // oxlint-disable-next-line typescript/only-throw-error -- TanStack Router's beforeLoad redirect contract: throw the Response redirect() returns, not an Error.
      throw redirect({ to: '/marketplace/$kind/$listingId', params: { kind: 'agents', listingId }, replace: true });
    }
  }
  // oxlint-disable-next-line typescript/only-throw-error -- same contract as above.
  throw redirect({ to: '/marketplace', search: kind === 'skills' ? { kind } : {}, replace: true });
}
