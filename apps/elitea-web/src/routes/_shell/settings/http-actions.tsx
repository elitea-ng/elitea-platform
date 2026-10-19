/**
 * `/settings/http-actions` -> `HttpActionsContent` page (T-5D).
 *
 * Same shape as `webhooks.tsx`: the route owns only the `Route` definition,
 * so the router's lazy code splitter can move the screen out of the initial
 * bundle.
 */
import { createFileRoute } from '@tanstack/react-router';

import { HttpActionsContent } from '@/pages/settings/HttpActions';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

export const Route = createFileRoute('/_shell/settings/http-actions')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: HttpActionsContent,
});
