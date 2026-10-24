/**
 * The projects a workspace can be bound to — the same list the sidebar's
 * project switcher shows (`useProjectOptions`, so one query, one cache key).
 */
import { useMemo } from 'react';

import { getConfig } from '@/shared/config';
import { useProjectOptions } from '@/widgets/sidebar';

/** A project a folder can be bound to (the shape `WorkspaceList` takes). */
export interface ProjectChoice {
  id: number;
  name: string;
}

export function useBindableProjects(): readonly ProjectChoice[] {
  const config = getConfig();
  const publicProjectId = config.status === 'ok' ? config.config.vite_public_project_id : '';
  const { projects } = useProjectOptions(publicProjectId);
  return useMemo(
    () => projects.filter((project) => !project.suspended).map((project) => ({ id: Number(project.id), name: project.name })),
    [projects],
  );
}
