/** `/marketplace/:kind/:listingId` -> one listing as a page (ADR-0051 decision 8). */
import { createFileRoute } from '@tanstack/react-router';

import { MarketplaceListing } from '@/pages/marketplace';

import { RouteError, RoutePending } from '../../-ui/RouteStatus';

export const Route = createFileRoute('/_shell/marketplace/$kind/$listingId')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: MarketplaceListing,
});
