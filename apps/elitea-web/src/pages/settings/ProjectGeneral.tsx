/**
 * Settings › General — the PROJECT section's first tab and the tab
 * `/settings` itself opens on.
 *
 * Ported from `EliteaUI/src/[fsd]/features/settings/ui/project-general/
 * ProjectGeneralContent.jsx`. This app had no General tab at all: the
 * reference's first PROJECT row, and its `DEFAULT_TAB`, simply did not
 * exist here. Two of its three sections had been grafted onto other tabs
 * instead — the project identity row onto Project Context, the AI
 * configuration summary onto AI Providers' "OpenAI Template" — and the third
 * (Agent & Pipeline Builder) was absent from the app entirely.
 *
 * Three accordions, all expanded, all left-chevron, in this order:
 *   GENERAL                   — avatar, name, teammate count
 *   AI CONFIGURATIONS         — the OpenAI base URL / server URL / project id
 *   AGENT & PIPELINE BUILDER  — one switch
 *
 * The measured column on a live deployment is 750px wide inside a 24px
 * gutter with 24px between panels, which is `body`'s `46.875rem` /
 * `1rem 1.5rem` / `2.375rem` bottom / `1.5rem` gap below — the reference's
 * own numbers.
 */
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useUpdateProjectInfoMutation } from '@/entities/project';
import { projectContextFeature, projectGeneralFeature, type SelectedProjectIcon } from '@/features/settings';
import { AccordionConstants } from '@/shared/lib/constants';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { usePermissionSet } from '@/widgets/sidebar';

const { ProjectParamsHeader } = projectContextFeature;
const { AgentPipelineBuilder, ProjectAIConfigurationSection } = projectGeneralFeature;

export interface ProjectGeneralProps {
  readonly projectId: string;
  readonly projectName: string;
}

export const ProjectGeneral = memo(function ProjectGeneral({
  projectId,
  projectName,
}: ProjectGeneralProps) {
  const permissionSet = usePermissionSet(projectId);
  const canEdit = permissionSet.has(PERMISSIONS.projectContext.edit);

  const updateProjectInfo = useUpdateProjectInfoMutation(projectId);

  /*
   * The project icon moved here WITH its writer. `ProjectContext.tsx` used to
   * own this mutation because it owned the header; leaving the mutation
   * behind would have shipped a header whose edit button opens a dialog that
   * saves nothing.
   *
   * The `url` travels with the `name`. Sending the name alone stores an
   * `icon_meta` the header cannot draw — it renders `icon_meta.url` and has
   * no way to resolve a name into an image.
   */
  const handleIconChange = useCallback(
    (icon: SelectedProjectIcon | null) => {
      updateProjectInfo.mutate(icon ? { name: icon.name, url: icon.url ?? null } : null);
    },
    [updateProjectInfo],
  );

  return (
    /* NO `DrawerPage` WRAPPER. The reference's root is the centring flex
     * column itself (`ProjectGeneralContent.jsx`'s `root`: `alignItems:
     * center`), with the header and body as its direct children. Wrapping
     * them in `DrawerPage` — a full-width box — defeats that: the wrapper
     * fills the column, so centring it moves nothing and the 750px body sits
     * flush left. Measured on the live page the body is centred in the
     * settings pane (x=843 of 436..2000); with the wrapper this app drew it
     * at x=460. */
    <Box sx={rootSx} data-testid="project-general-body">
      <DrawerPageHeader title={t('settings.projectGeneral.title', 'General')} showBorder />
      <Box sx={bodySx}>
        <BasicAccordion
          data-testid="project-general-section"
          showMode={AccordionConstants.AccordionShowMode.LeftMode}
          defaultExpanded
          slotSx={{ details: detailsSx }}
          items={[
            {
              title: t('settings.projectGeneral.general', 'General'),
              content: (
                <ProjectParamsHeader
                  projectId={projectId}
                  projectName={projectName}
                  canEdit={canEdit}
                  onIconChange={handleIconChange}
                />
              ),
            },
          ]}
        />
        <BasicAccordion
          data-testid="ai-configurations"
          showMode={AccordionConstants.AccordionShowMode.LeftMode}
          defaultExpanded
          slotSx={{ details: detailsSx }}
          items={[
            {
              title: t('settings.projectGeneral.aiConfigurations', 'AI Configurations'),
              content: <ProjectAIConfigurationSection projectId={projectId} />,
            },
          ]}
        />
        <BasicAccordion
          data-testid="agent-pipeline-builder-section"
          showMode={AccordionConstants.AccordionShowMode.LeftMode}
          defaultExpanded
          slotSx={{ details: detailsSx }}
          items={[
            {
              title: t(
                'settings.projectGeneral.agentPipelineBuilder.section',
                'Agent & Pipeline Builder',
              ),
              content: <AgentPipelineBuilder canEdit={canEdit} />,
            },
          ]}
        />
      </Box>
    </Box>
  );
});

const rootSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  height: '100%',
  alignItems: 'center',
};

const bodySx: SxProps<Theme> = {
  flex: 1,
  overflow: 'auto',
  minHeight: 0,
  padding: '1rem 1.5rem',
  paddingBottom: '2.375rem',
  display: 'flex',
  flexDirection: 'column',
  gap: '1.5rem',
  width: '100%',
  maxWidth: '46.875rem',
};

/* Live page: `MuiAccordionDetails-root` has `padding: 0 0 0 36px` here — the
 * body lines up under the summary's label, not under its chevron. */
const detailsSx: SxProps<Theme> = { padding: '0 0 0 2.25rem' };
