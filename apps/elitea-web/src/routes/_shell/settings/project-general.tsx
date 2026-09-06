/**
 * `/settings/project-general` -> Settings › General.
 *
 * The reference's `DEFAULT_TAB` and the first row of the PROJECT section
 * (`pages/settings/index.jsx:56-60,130`). `/settings` and every unrecognised
 * settings slug now land here; `shared/ui/settings/SettingsRedirect.tsx`'s
 * `DEFAULT_SETTINGS_TAB` is the one place that names it.
 *
 * Shaped after `project-params.tsx`: the route reads the selected project and
 * holds the page at `RoutePending` until there is one, so the page itself
 * never has to render an "unknown project" state.
 */
import { createFileRoute } from '@tanstack/react-router';

import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';
import { ProjectGeneral } from '@/pages/settings/ProjectGeneral';
import { useSelectedProjectStore } from '@/widgets/app-shell';

export const Route = createFileRoute('/_shell/settings/project-general')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: ProjectGeneralPage,
});

function ProjectGeneralPage() {
  const project = useSelectedProjectStore((s) => s.project);
  const projectId = project?.id ?? '';
  const projectName = project?.name ?? '';

  if (!projectId) {
    return <RoutePending />;
  }

  return (
    <ProjectGeneral
      projectId={projectId}
      projectName={projectName}
    />
  );
}
