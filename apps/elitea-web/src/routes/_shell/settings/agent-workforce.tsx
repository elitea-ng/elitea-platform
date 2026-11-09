/**
 * `/settings/agent-workforce` -> `AgentWorkforceContent` page (issue 1617).
 *
 * Same shape as `webhooks.tsx`: the route owns nothing but the `Route`
 * definition, so the router's lazy code splitter can move the whole screen
 * out of the initial bundle.
 */
import { createFileRoute } from '@tanstack/react-router';

import { AgentWorkforceContent } from '@/pages/settings/AgentWorkforce';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

export const Route = createFileRoute('/_shell/settings/agent-workforce')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: AgentWorkforceContent,
});
