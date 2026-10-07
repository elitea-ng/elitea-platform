import { router } from 'expo-router';

import { groupProjects, useProjects } from '@/api/queries/projects';
import { ProjectsSheet } from '@/features/projects/ProjectsSheet';
import { useActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { selectProject } from '@/state/projectSelection';
import { usePersonalProjectId } from '@/state/selfIdentity';
import { useSession } from '@/state/session';

/**
 * Project switcher (a sheet with detents) for the ACTIVE workspace: the workspace it belongs to
 * on top, then the personal project, then the team projects by NAME (the client-tagged project
 * list; suspended ones are hidden). The choice is remembered per workspace.
 */
export default function ProjectsScreen() {
  const projects = useProjects();
  const selected = useSession((s) => s.projectId);
  const personalId = usePersonalProjectId();
  const workspace = useActiveWorkspace();
  const groups = groupProjects(projects.data ?? [], personalId);
  return (
    <ProjectsSheet
      workspace={workspace}
      loading={projects.isLoading}
      failed={projects.error != null}
      onRetry={() => void projects.refetch()}
      personal={groups.personal}
      team={groups.team}
      selectedId={selected}
      onSelect={(id) => {
        void selectProject(id);
        if (router.canGoBack()) router.back();
      }}
      onSwitchWorkspace={() => router.replace('/workspaces')}
    />
  );
}
