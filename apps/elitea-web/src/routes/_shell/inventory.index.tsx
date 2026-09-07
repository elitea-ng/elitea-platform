/**
 * INV-001 `/inventory` — the project's Inventory applications, reached without
 * a URL someone had to be handed.
 *
 * THE LEGACY HAD NO SUCH SCREEN. Its bundle was always entered with a toolkit
 * already in the path (`/app/ui_host/inventory/ui/{project}/{toolkit}`),
 * because pylon's provider hub linked to it. This application has its own
 * routes, so the route resolves its own toolkit — and a project can hold
 * several Inventory toolkits over different buckets, so it cannot guess.
 *
 * Three states, and the middle one is why this is not a redirect: exactly one
 * toolkit renders it, several offer the choice, none says so. A redirect on
 * "exactly one" would make the URL depend on how many toolkits a project
 * happens to have, so Back would land somewhere different for two projects.
 *
 * `inventory.index.tsx` AND NOT `inventory.tsx`. As `inventory.tsx` this file
 * becomes the PARENT of `inventory.$toolkitId.tsx`, and a parent that renders
 * its own UI without an `<Outlet/>` swallows its children: `/inventory/999999`
 * would match, render THIS component, and show the project's own graph as
 * though the id had been honoured. Nothing fails and the screen looks right.
 * The DeepWiki port found that the hard way; this file starts on the other
 * side of it.
 */
import { createFileRoute, useNavigate } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';

import { useInventoryToolkits } from '@/entities/inventory';
import { hasBackendCapability } from '@/shared/config/backendCapabilities';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { RouteError, RoutePending } from '../-ui/RouteStatus';
import { InventoryToolkit } from '@/pages/inventory/InventoryToolkit';

export const Route = createFileRoute('/_shell/inventory/')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: InventoryIndexRoute,
});

function InventoryIndexRoute(): React.JSX.Element | null {
  const projectId = useSelectedProjectStore((state) => state.project?.id ?? '');
  const query = useInventoryToolkits(projectId);
  const navigate = useNavigate();

  if (!hasBackendCapability('inventory')) return null;
  if (query.isPending) return <RoutePending />;

  if (query.isError) {
    return (
      <Box data-testid="inventory-toolkits-error" sx={{ p: '1.5rem' }}>
        <BannerMessage
          variant="error"
          message={t('inventory.toolkitsFailed', 'The Inventory toolkits for this project could not be listed.')}
        />
      </Box>
    );
  }

  const toolkits = query.data;

  if (toolkits.length === 0) {
    // NOT an empty graph. "This project has no Inventory toolkit" and "this
    // inventory holds no entities" are different facts, and only the first one
    // tells the user what to do next.
    return (
      <Box data-testid="inventory-no-toolkits" sx={{ p: '1.5rem' }}>
        <NoResultsMessage
          title={t('inventory.noToolkitsTitle', 'No Inventory toolkit')}
          description={t(
            'inventory.noToolkits',
            'This project has no Inventory toolkit. Add one to build and browse a knowledge graph.',
          )}
        />
      </Box>
    );
  }

  if (toolkits.length === 1 && toolkits[0]) {
    return <InventoryToolkit projectId={projectId} toolkitId={toolkits[0].id} />;
  }

  return (
    <Box data-testid="inventory-toolkit-chooser" sx={{ p: '1.5rem' }}>
      <Typography variant="headingMedium" sx={{ mb: 1 }}>
        {t('inventory.chooseToolkit', 'Choose an inventory')}
      </Typography>
      <List>
        {toolkits.map((toolkit) => (
          <ListItemButton
            key={toolkit.id}
            onClick={() => {
              void navigate({ to: '/inventory/$toolkitId', params: { toolkitId: toolkit.id } });
            }}
          >
            <ListItemText primary={toolkit.name} />
          </ListItemButton>
        ))}
      </List>
    </Box>
  );
}
