import { useMemo } from 'react';

import type { ForkTargetProject } from '@/features/agent-lifecycle';
import { getConfig } from '@/shared/config';
import { useProjectOptions } from '@/widgets/sidebar';

/**
 * The projects a pipeline fork may copy INTO.
 *
 * Same body as `pages/agents/lib/useForkTargetProjects.ts`, duplicated for the
 * reason `pages/pipelines/lib/useSelectedProjectId.ts` gives for its own twin:
 * the two pages are siblings, and a cross-page import is the edge that becomes
 * a cycle the first time the other direction is wanted. Everything that
 * DECIDES anything lives in `features/agent-lifecycle`; what is duplicated is
 * only the widget lookup a feature is not allowed to make
 * (`no-upward-from-features`).
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
