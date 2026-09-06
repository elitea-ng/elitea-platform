/**
 * ROUTE-019 `/pipelines/:tab` -> `Pipelines`. Query params PARAM-052/054/056
 * (`isFromCreation`, `sort_by`, `sort_order`) — inherited by `$tab/$agentId`
 * (adds `history_run_id`, PARAM-059). See agents' `$tab.tsx` for the
 * `ExclusiveOutlet`/inheritance rationale (identical shape here).
 */
import { createFileRoute } from '@tanstack/react-router';

import { Pipelines } from '@/pages/pipelines/Pipelines';

import { ExclusiveOutlet } from '../../-ui/ExclusiveOutlet';
import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

export const Route = createFileRoute('/_shell/pipelines/$tab')({
  validateSearch: pickParams('isFromCreation', 'sort_by', 'sort_order', 'view'),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: PipelinesRoute,
});

function PipelinesRoute() {
  return (
    <ExclusiveOutlet>
      <Pipelines />
    </ExclusiveOutlet>
  );
}
