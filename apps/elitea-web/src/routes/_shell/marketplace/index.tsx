/**
 * `/marketplace` -> `Marketplace` (ADR-0051): shared agents and skills as
 * listings. `?kind=agents|skills` picks the kind; the search, categories and
 * sort are URL params too (`pages/marketplace/filters.ts`), so a filtered view can be shared.
 */
import { createFileRoute } from '@tanstack/react-router';

import { Marketplace } from '@/pages/marketplace';

import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

export const Route = createFileRoute('/_shell/marketplace/')({
  validateSearch: pickParams(
    'kind',
    'query',
    'category',
    'tag',
    'publisher_id',
    'max_tier',
    'requires',
    'min_rating',
    'hired_here',
    'sort_by',
  ),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: Marketplace,
});
