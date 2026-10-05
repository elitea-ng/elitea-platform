/**
 * `/settings/devices` -> `DevicesContent` page (ADR-0025 WP3): the caller's own
 * signed-in mobile and desktop apps.
 *
 * Same shape as `webhooks.tsx`: the route owns nothing but the `Route`
 * definition, so the router's lazy code splitter can move the whole screen
 * out of the initial bundle (`DevicesContent` is not exported from here).
 */
import { createFileRoute } from '@tanstack/react-router';

import { DevicesContent } from '@/pages/settings/Devices';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

export const Route = createFileRoute('/_shell/settings/devices')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: DevicesContent,
});
