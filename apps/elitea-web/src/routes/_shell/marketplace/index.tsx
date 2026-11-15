/**
 * `/marketplace` -> `Marketplace` (ADR-0051): shared agents and skills as
 * listings. `?kind=agents|skills` picks the kind; the search, categories and
 * sort are the Browse sidebar's own state.
 */
import { createFileRoute } from '@tanstack/react-router';

import { Marketplace } from '@/pages/marketplace';

import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

export const Route = createFileRoute('/_shell/marketplace/')({
  validateSearch: pickParams('kind'),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: Marketplace,
});
