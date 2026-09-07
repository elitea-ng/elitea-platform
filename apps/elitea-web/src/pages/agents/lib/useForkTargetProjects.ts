import { useMemo } from 'react';

import type { ForkTargetProject } from '@/features/agent-lifecycle';
import { getConfig } from '@/shared/config';
import { useProjectOptions } from '@/widgets/sidebar';

/**
 * The projects a fork may copy INTO.
 *
 * `useProjectOptions` lives in `widgets/sidebar` and `features/agent-lifecycle`
 * may not import a widget (`no-upward-from-features`), so the page owns this
 * join and passes the list down. It is a page-side hook rather than a feature
 * one for exactly that reason.
 *
 * The list route is served under the PUBLIC project (`GET /projects/project/
 * default/{publicProjectId}`), not under the caller's own — the id below is
 * the public one, and it is `''` while config has not resolved, which leaves
 * the query disabled instead of sending a request with an empty path segment.
 */
export function useForkTargetProjects(selectedProjectId: string | undefined): readonly ForkTargetProject[] {
  const config = getConfig();
  const publicProjectId = config.status === 'ok' ? config.config.vite_public_project_id : '';
  const { projects } = useProjectOptions(publicProjectId, selectedProjectId);
  return useMemo(
    () => projects.map((project) => ({ id: String(project.id), name: project.name })),
    [projects],
  );
}
