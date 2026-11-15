/**
 * The old `?agentId=` links name the PUBLIC agent; a listing page is keyed by
 * the LISTING id. `findListingIdByEntity` asks the search route for the listing
 * of that entity. It answers the id, `null` when the agent has no listing (the
 * link is dead, so it goes home), and throws when the lookup fails, so a caller
 * can tell "no listing" from "could not ask" and retry the second.
 */
import { searchMarketplaceListings } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceListingPage } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

export async function findListingIdByEntity(projectId: string, entityId: string): Promise<string | null> {
  const response = await searchMarketplaceListings(projectId, { entity_id: entityId, kind: 'agent', limit: 1 });
  return (unwrapBody(response) as MarketplaceListingPage).rows[0]?.id ?? null;
}
