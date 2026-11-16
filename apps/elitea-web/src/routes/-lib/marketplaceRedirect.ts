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
 * entity. Only "this agent has no listing" lands on the Marketplace home. When
 * the project is not selected yet or the lookup fails, the link is NOT dropped:
 * it goes to `/marketplace?agentId=`, and the page finishes the lookup, with a
 * retry, once it can (`pages/marketplace/useLegacyAgentLink.ts`).
 */
import { redirect } from '@tanstack/react-router';

import { findListingIdByEntity, kindFromSearch } from '@/pages/marketplace';
import { selectProjectId } from '@/pages/skills/lib/useSelectedProjectId';

export interface LegacyCatalogSearch {
  readonly tab?: string | undefined;
  readonly agentId?: string | undefined;
}

type Lookup = { readonly found: string | null } | { readonly pending: true };

async function lookup(projectId: string | undefined, entityId: string): Promise<Lookup> {
  if (projectId === undefined) return { pending: true };
  try {
    return { found: await findListingIdByEntity(projectId, entityId) };
  } catch {
    return { pending: true };
  }
}

export async function redirectToMarketplace(search: LegacyCatalogSearch, context: unknown): Promise<never> {
  const kind = kindFromSearch(search.tab);
  const agentId = search.agentId ?? '';
  if (agentId !== '') {
    const result = await lookup(selectProjectId(context), agentId);
    if ('pending' in result) {
      // oxlint-disable-next-line typescript/only-throw-error -- TanStack Router's beforeLoad redirect contract: throw the Response redirect() returns, not an Error.
      throw redirect({ to: '/marketplace', search: { agentId }, replace: true });
    }
    if (result.found !== null) {
      // oxlint-disable-next-line typescript/only-throw-error -- same contract as above.
      throw redirect({
        to: '/marketplace/$kind/$listingId',
        params: { kind: 'agents', listingId: result.found },
        replace: true,
      });
    }
  }
  // oxlint-disable-next-line typescript/only-throw-error -- same contract as above.
  throw redirect({ to: '/marketplace', search: kind === 'skills' ? { kind } : {}, replace: true });
}
