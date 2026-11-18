/** ROUTE-020 `/pipelines/:tab/:agentId` -> `EditPipeline` (nav-block — Wave-2 concern). Adds PARAM-059 `history_run_id`. */
import { createFileRoute, Outlet } from '@tanstack/react-router';

import { EditPipeline } from '@/pages/pipelines/EditPipeline';

import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

export const Route = createFileRoute('/_shell/pipelines/$tab/$agentId')({
  validateSearch: pickParams('history_run_id', 'review_delegation'),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: EditPipelineRoute,
});

function EditPipelineRoute() {
  return (
    <>
      <EditPipeline />
      <Outlet />
    </>
  );
}
