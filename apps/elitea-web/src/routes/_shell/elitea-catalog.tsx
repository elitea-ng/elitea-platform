/**
 * `/elitea-catalog` — now a redirect source only: the Catalog is the
 * Marketplace (ADR-0051). `?tab=skills` becomes `?kind=skills`, and the shared
 * `?agentId=` deep link becomes the agent's listing page.
 */
import { createFileRoute } from '@tanstack/react-router';

import { pickParams } from '../-search/params';
import { redirectToMarketplace } from '../-lib/marketplaceRedirect';

export const Route = createFileRoute('/_shell/elitea-catalog')({
  validateSearch: pickParams('tab', 'agentId'),
  beforeLoad: ({ search, context }) => redirectToMarketplace(search, context),
});
