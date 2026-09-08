/**
 * ROUTE-011 `/agents/:tab` -> `Applications` (spec §8.1). Query params
 * PARAM-001/003/005/007/009/011/013/015/017/019 (`destTab`,
 * `edited_participant_id`, `isFromCreation`, `mcp`, `newToolkitId`,
 * `return_url`, `sort_by`, `sort_order`, `source_application_id`,
 * `viewMode`) — inherited by `$tab/$agentId` (adds `history_run_id` there,
 * PARAM-002/58 pattern; see that file).
 *
 * `ExclusiveOutlet`: old app's Applications (list) and EditApplication
 * (detail) are sibling, mutually-exclusive full-page routes
 * (`ProtectedRoutes.jsx`'s flat array) — see `-ui/ExclusiveOutlet.tsx`'s
 * header for why TanStack's file-based nesting (`/agents/$tab` is the
 * structural parent of `/agents/$tab/$agentId` because they share a path
 * prefix) needs this to reproduce that.
 */
import { createFileRoute } from '@tanstack/react-router';

import { Applications } from '@/pages/agents/Applications';

import { ExclusiveOutlet } from '../../-ui/ExclusiveOutlet';
import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

export const Route = createFileRoute('/_shell/agents/$tab')({
  validateSearch: pickParams(
    'destTab',
    'edited_participant_id',
    'isFromCreation',
    'mcp',
    'newToolkitId',
    'return_url',
    'sort_by',
    'sort_order',
    'source_application_id',
    'query',
    'view',
    'viewMode',
  ),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: ApplicationsRoute,
});

function ApplicationsRoute() {
  return (
    <ExclusiveOutlet>
      <Applications />
    </ExclusiveOutlet>
  );
}
