/**
 * Copy to project (ADR-0050 decision 1), offered only on an agent in the
 * viewer's own personal project. The page joins the two things the feature
 * may not read itself: the viewer's personal project (router context) and
 * the team projects (`widgets/sidebar`, through `useForkTargetProjects`).
 *
 * The editor stays on the personal agent afterwards and says where the copy
 * went: the editor route reads the SELECTED project, so opening the copy
 * here would look it up in the wrong one.
 */
import type { ReactNode } from 'react';

import { CopyToProjectButton, type CopyTargetProject } from '@/features/agent-proposals';
import { t } from '@/shared/i18n';
import { useRailViewer } from '@/shared/ui/EntityRail';

export interface PersonalAgentCopyButtonProps {
  readonly projectId: string | undefined;
  readonly applicationId: string | undefined;
  readonly name: string | undefined;
  /** Team projects, the editor's fork targets (the personal project is never one). */
  readonly projects: readonly CopyTargetProject[];
  readonly onNotice: (message: string) => void;
}

export function PersonalAgentCopyButton({ projectId, applicationId, name, projects, onNotice }: PersonalAgentCopyButtonProps): ReactNode {
  const { personalProjectId } = useRailViewer();
  const id = Number(applicationId);
  if (projectId === undefined || personalProjectId === undefined || projectId !== personalProjectId || !Number.isInteger(id) || id <= 0) return null;
  return (
    <CopyToProjectButton
      projectId={projectId}
      applicationId={id}
      agentName={name ?? ''}
      projects={projects}
      onCopied={(targetProjectId, _copyId, memoryCopied) => {
        const project = projects.find((candidate) => candidate.id === targetProjectId)?.name ?? targetProjectId;
        onNotice(
          t('pages.agents.editApplication.copied', 'A copy was created in {{project}} with {{count}} memory entries.', {
            project,
            count: memoryCopied,
          }),
        );
      }}
    />
  );
}
