/**
 * INV-001 `/inventory/$toolkitId` — the workspace for one Inventory toolkit.
 *
 * THE TOOLKIT IS IN THE PATH AND THE PROJECT IS NOT. That split is the legacy
 * addressing rewritten in this application's terms: its bundle took both out
 * of `/app/ui_host/inventory/ui/{project}/{toolkit}`, and here the project is
 * whatever the project switcher has selected — the rule every other `_shell`
 * route follows. A project can hold several Inventory toolkits over different
 * buckets, so the toolkit is the graph's address and cannot be inferred.
 */
import { createFileRoute } from '@tanstack/react-router';

import { hasBackendCapability } from '@/shared/config/backendCapabilities';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { RouteError, RoutePending } from '../-ui/RouteStatus';
import { InventoryToolkit } from '@/pages/inventory/InventoryToolkit';

export const Route = createFileRoute('/_shell/inventory/$toolkitId')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: InventoryToolkitRoute,
});

function InventoryToolkitRoute(): React.JSX.Element | null {
  const { toolkitId } = Route.useParams();
  const projectId = useSelectedProjectStore((state) => state.project?.id ?? '');

  if (!hasBackendCapability('inventory')) return null;
  return <InventoryToolkit projectId={projectId} toolkitId={toolkitId} />;
}
