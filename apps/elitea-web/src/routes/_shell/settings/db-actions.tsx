/**
 * `/settings/db-actions` -> `DbActionsContent` page (T-5E 5e-2).
 *
 * Same shape as `http-actions.tsx`: the route owns only the `Route` definition,
 * so the router's lazy code splitter can move the screen out of the initial
 * bundle.
 */
import { createFileRoute } from '@tanstack/react-router';

import { DbActionsContent } from '@/pages/settings/DbActions';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

export const Route = createFileRoute('/_shell/settings/db-actions')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: DbActionsContent,
});
