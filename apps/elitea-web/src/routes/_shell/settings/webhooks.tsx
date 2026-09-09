/**
 * `/settings/webhooks` -> `WebhooksContent` page (#876).
 *
 * Same shape as `secrets.tsx`: the route owns nothing but the `Route`
 * definition and a thin wrapper component, so the router's lazy code
 * splitter can still move the whole screen out of the initial bundle
 * (`WebhooksContent` is not itself exported from this file).
 */
import { createFileRoute } from '@tanstack/react-router';

import { WebhooksContent } from '@/pages/settings/Webhooks';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

export const Route = createFileRoute('/_shell/settings/webhooks')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: WebhooksContent,
});
