/**
 * DWIKI-001 `/deepwiki` — the project's wikis, reached without a URL someone
 * had to be handed.
 *
 * THE LEGACY HAD NO SUCH SCREEN. The vendored bundle is always entered with a
 * toolkit already in its path (`/app/ui_host/deepwiki/ui/{project}/{toolkit}`,
 * DeepWikiApp.jsx:756-788), because the legacy provider hub linked to it. This
 * app has a nav rail instead, so the route has to resolve its own toolkit —
 * and a project can hold several wiki toolkits pointed at different
 * repositories, so it cannot simply guess.
 *
 * Three states, and the middle one is the reason this is not a redirect:
 * exactly one toolkit renders it, several offer the choice, none says so. A
 * redirect on "exactly one" would make the URL depend on how many toolkits a
 * project happens to have, so the browser's Back button would land somewhere
 * different for two projects.
 *
 * `deepwiki.index.tsx` AND NOT `deepwiki.tsx`, which is not a naming
 * preference. As `deepwiki.tsx` this file became the PARENT of
 * `deepwiki.$toolkitId.tsx`, and a parent that renders its own UI without an
 * `<Outlet/>` swallows its children: `/deepwiki/999999` matched, rendered THIS
 * component, and showed the project's own wiki as though the id in the URL had
 * been honoured. Nothing failed — the screen looked right — and the
 * toolkit-addressed route had never rendered at all. Caught by the journey that
 * asks for a toolkit which does not exist and expects to be told so.
 */
import { createFileRoute, useNavigate } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';

import { useWikiToolkits } from '@/entities/wiki';
import { hasBackendCapability } from '@/shared/config/backendCapabilities';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { RouteError, RoutePending } from '../-ui/RouteStatus';
import { DeepWikiToolkit } from '@/pages/deepwiki/DeepWikiToolkit';

export const Route = createFileRoute('/_shell/deepwiki/')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: DeepWikiIndexRoute,
});

function DeepWikiIndexRoute(): React.JSX.Element | null {
  const projectId = useSelectedProjectStore((state) => state.project?.id ?? '');
  const query = useWikiToolkits(projectId);
  const navigate = useNavigate();

  if (!hasBackendCapability('deepwiki')) return null;
  if (query.isPending) return <RoutePending />;

  if (query.isError) {
    return (
      <Box data-testid="deepwiki-toolkits-error" sx={{ p: '1.5rem' }}>
        <BannerMessage
          variant="error"
          message={t('deepwiki.toolkitsFailed', 'The wiki toolkits for this project could not be listed.')}
        />
      </Box>
    );
  }

  const toolkits = query.data;

  if (toolkits.length === 0) {
    // NOT an empty wiki list. "This project has no wiki toolkit" and "this
    // wiki has no pages" are different facts, and only the first one tells the
    // user what to do next.
    return (
      <Box data-testid="deepwiki-no-toolkits" sx={{ p: '1.5rem' }}>
        <NoResultsMessage
          title={t('deepwiki.noToolkitsTitle', 'No wiki toolkit')}
          description={t(
            'deepwiki.noToolkits',
            'This project has no wiki toolkit. Add one to generate and browse wikis.',
          )}
        />
      </Box>
    );
  }

  if (toolkits.length === 1 && toolkits[0]) {
    return <DeepWikiToolkit projectId={projectId} toolkitId={toolkits[0].id} />;
  }

  return (
    <Box data-testid="deepwiki-toolkit-chooser">
      <Typography variant="headingMedium" sx={{ mb: 1 }}>
        {t('deepwiki.chooseToolkit', 'Choose a wiki')}
      </Typography>
      <List>
        {toolkits.map((toolkit) => (
          // The Link WRAPS the button rather than being its `component`: this
          // app's MUI build does not expose the polymorphic `component` prop on
          // ListItemButton's types, and a plain onClick would lose the middle
          // click and the copyable href a router Link gives for free.
          <ListItemButton
            key={toolkit.id}
            onClick={() => {
              void navigate({ to: '/deepwiki/$toolkitId', params: { toolkitId: toolkit.id } });
            }}
          >
            <ListItemText primary={toolkit.name} />
          </ListItemButton>
        ))}
      </List>
    </Box>
  );
}
