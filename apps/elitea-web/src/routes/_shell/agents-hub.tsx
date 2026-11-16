/**
 * `/agents-hub` — a redirect source, as it has been since the Catalog replaced
 * the hub page, and now into the Marketplace (ADR-0051). The one link shape
 * that reaches this path in the wild is the shared `?agentId=` deep link, so
 * both keys survive validation here and `redirectToMarketplace` carries them.
 */
import { createFileRoute } from '@tanstack/react-router';

import { pickParams } from '../-search/params';
import { redirectToMarketplace } from '../-lib/marketplaceRedirect';

export const Route = createFileRoute('/_shell/agents-hub')({
  validateSearch: pickParams('agentId', 'tab'),
  beforeLoad: ({ search, context }) => redirectToMarketplace(search, context),
});
