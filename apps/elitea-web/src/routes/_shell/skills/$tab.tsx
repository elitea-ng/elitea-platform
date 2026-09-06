/**
 * ROUTE-015 `/skills/:tab` -> `Skills`, wrapped in `SkillsGuard` (PERM-058).
 * `beforeLoad` here also gates `$tab/$skillId` (and its `:version` child):
 * TanStack Router runs every matched ancestor's `beforeLoad`, so a deep
 * link straight to `/skills/all/some-id` still hits this guard.
 * Query params PARAM-088/090/092 (`newSkillId`, `return_url`,
 * `source_application_id`) — inherited by `$tab/$skillId` (PARAM-089/091/093
 * declare the identical set, no additions, so the child needs no extra
 * `validateSearch` of its own).
 */
import { createFileRoute } from '@tanstack/react-router';

import { Skills } from '@/pages/skills/Skills';

import { skillsGuardBeforeLoad } from '../../-guards/skillsGuard';
import { ExclusiveOutlet } from '../../-ui/ExclusiveOutlet';
import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

export const Route = createFileRoute('/_shell/skills/$tab')({
  validateSearch: pickParams('newSkillId', 'return_url', 'source_application_id', 'view'),
  beforeLoad: skillsGuardBeforeLoad,
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: SkillsRoute,
});

function SkillsRoute() {
  return (
    <ExclusiveOutlet>
      <Skills />
    </ExclusiveOutlet>
  );
}
