/**
 * ROUTE-012 `/agents/:tab/:agentId` -> `EditApplication` (spec §8.1,
 * nav-block when `viewMode===Owner && dirty` — Wave-2 concern). Adds
 * PARAM-058 `history_run_id` on top of the 10 keys `$tab.tsx` already
 * declares (inherited — see that file's header).
 */
import { createFileRoute, Outlet } from '@tanstack/react-router';

import { EditApplication } from '@/pages/agents/EditApplication';

import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

export const Route = createFileRoute('/_shell/agents/$tab/$agentId')({
  validateSearch: pickParams('history_run_id', 'review_delegation'),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: EditApplicationRoute,
});

function EditApplicationRoute() {
  return (
    <>
      <EditApplication />
      <Outlet />
    </>
  );
}
