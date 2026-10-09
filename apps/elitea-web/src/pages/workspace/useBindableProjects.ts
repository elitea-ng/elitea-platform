/**
 * The projects a workspace can be bound to — the same list the sidebar's
 * project switcher shows (`useProjectOptions`, so one query, one cache key).
 */
import { useMemo } from 'react';

import type { ProjectChoice } from '@/features/workspace';
import { getConfig } from '@/shared/config';
import { useProjectOptions } from '@/widgets/sidebar';

export function useBindableProjects(): readonly ProjectChoice[] {
  const config = getConfig();
  const publicProjectId = config.status === 'ok' ? config.config.vite_public_project_id : '';
  const { projects } = useProjectOptions(publicProjectId);
  return useMemo(
    () => projects.filter((project) => !project.suspended).map((project) => ({ id: Number(project.id), name: project.name })),
    [projects],
  );
}
