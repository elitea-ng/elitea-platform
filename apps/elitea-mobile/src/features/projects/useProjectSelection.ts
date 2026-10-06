import { useEffect } from 'react';

import { resolveProjectSelection, useProjects, type Project } from '@/api/queries/projects';
import { selectProject } from '@/state/projectSelection';
import { usePersonalProjectId } from '@/state/selfIdentity';
import { useSession } from '@/state/session';

export interface ProjectSelection {
  projectId: number | null;
  project: Project | null;
  /** Projects loaded and none is usable (all suspended or none at all). */
  noProject: boolean;
  isLoading: boolean;
  error: unknown;
}

/**
 * Keeps the session's project valid against the server's list: a stale,
 * unknown or suspended selection falls back to the personal project, else
 * the first usable one. The choice is per workspace (it lives in the
 * workspace's own secure-store namespace).
 */
export function useProjectSelection(): ProjectSelection {
  const projects = useProjects();
  const projectId = useSession((s) => s.projectId);
  const personal = usePersonalProjectId();
  const resolved = projects.data ? resolveProjectSelection(projects.data, projectId, personal) : projectId;

  useEffect(() => {
    if (projects.data && resolved !== null && resolved !== projectId) {
      void selectProject(resolved);
    }
  }, [projects.data, resolved, projectId]);

  return {
    projectId: resolved,
    project: projects.data?.find((p) => p.id === resolved) ?? null,
    noProject: projects.data !== undefined && resolved === null,
    isLoading: projects.isLoading,
    error: projects.error,
  };
}
