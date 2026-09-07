/**
 * One Inventory toolkit's workspace, shared by `/inventory` and
 * `/inventory/$toolkitId`.
 *
 * Both entry points resolve a toolkit and then render exactly the same thing,
 * and a second copy of that resolution is a second place for the error
 * branches to drift.
 *
 * IT LIVES HERE AND NOT UNDER `routes/`. `scripts/build-route-wiring-map.mjs`
 * walks every file under `src/routes` and counts it as a route, so a
 * `-inventory/` hidden directory would invent a route domain with one entry —
 * the trap the DeepWiki port recorded when it moved its own resolver here.
 *
 * It FETCHES, which a page does not normally do (spec §3): resolving the
 * toolkit is what both routes need done before anything can be rendered, and
 * it uses the entity's own hook rather than an ad-hoc request, which is the
 * sanctioned shape.
 */
import Box from '@mui/material/Box';

import { useInventoryToolkit } from '@/entities/inventory';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { InventoryWorkspace } from '@/widgets/inventory';

import { RoutePending } from '@/routes/-ui/RouteStatus';

export interface InventoryToolkitProps {
  readonly projectId: string;
  readonly toolkitId: string;
}

export function InventoryToolkit({
  projectId,
  toolkitId,
}: InventoryToolkitProps): React.JSX.Element {
  const query = useInventoryToolkit(projectId, toolkitId);

  if (query.isPending) return <RoutePending />;

  // A toolkit that cannot be read is NOT an empty graph. Rendering the
  // workspace here would say "this inventory has no entities" about a graph
  // whose bucket this screen never learned.
  if (query.isError) {
    return (
      <Box data-testid="inventory-toolkit-error" sx={{ p: '1.5rem' }}>
        <BannerMessage
          variant="error"
          message={t(
            'inventory.toolkitFailed',
            'This Inventory toolkit could not be loaded, so its knowledge graph cannot be read.',
          )}
        />
      </Box>
    );
  }

  return (
    <InventoryWorkspace
      projectId={projectId}
      toolkitId={toolkitId}
      settings={query.data.settings}
      // The WHOLE ROW, because saving a source list is a PUT that replaces the
      // resource: sending only the settings would clear every other field the
      // toolkit carries.
      toolkit={query.data.toolkit}
    />
  );
}
