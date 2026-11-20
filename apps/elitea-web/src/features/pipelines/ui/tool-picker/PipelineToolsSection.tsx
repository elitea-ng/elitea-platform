import { memo, useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { useIsMcpVisible } from '../../api/useIsMcpVisible';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import {
  collectToolReferences,
  groupAttachmentsByKind,
} from '../../lib/tool-picker/pipelineToolUsage.helpers';
import { PICKER_KINDS } from '../../lib/tool-picker/toolPicker.constants';
import type {
  PickerToolEntry,
  PipelineYamlDocument,
  ToolValidationOverride,
  ToolkitListRow,
} from '../../lib/tool-picker/toolPicker.types';
import { useIsVersionLocked } from '../../lib/tool-picker/useIsVersionLocked';
import { PipelineToolCard } from './PipelineToolCard';

export interface PipelineToolsSectionProps {
  readonly tools?: readonly PickerToolEntry[] | undefined;
  readonly yamlJsonObject?: PipelineYamlDocument;
  readonly versionStatus?: string | undefined;
  readonly attachmentToolkitId?: number | string | undefined;
  readonly disabled?: boolean | undefined;
  readonly onSelectNode?: ((nodeId: string) => void) | undefined;
  readonly onRemoveTool?: ((tool: PickerToolEntry) => void) | undefined;
  readonly entityProjectId?: string | undefined;
  readonly getToolValidation?: ((tool: PickerToolEntry) => ToolValidationOverride | null | undefined) | undefined;
  readonly onMcpLoginClick?: ((mcp: PickerToolEntry | ToolkitListRow) => void) | undefined;
  readonly modulesSlot?: ReactNode | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

const contentSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
  paddingTop: theme.spacing(1.5),
});

const groupSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1),
});

const groupTitleSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
  textTransform: 'uppercase',
});

/**
 * Read-only list (toolkits, MCPs, agents, then pipelines) of what the pipeline nodes use.
 * Toolkits, MCPs, agents and pipelines are picked in the nodes; "Used by" opens a node's panel.
 */
export const PipelineToolsSection = memo(function PipelineToolsSection({
  tools = [],
  yamlJsonObject,
  versionStatus,
  attachmentToolkitId,
  disabled = false,
  onSelectNode,
  onRemoveTool,
  entityProjectId,
  getToolValidation,
  onMcpLoginClick,
  modulesSlot,
  sx,
}: PipelineToolsSectionProps): ReactNode {
  const isVersionLocked = useIsVersionLocked(versionStatus);
  const canRemoveUnused = !disabled && !isVersionLocked && Boolean(onRemoveTool);
  const isMcpVisible = useIsMcpVisible();
  const selectedProjectId = useSelectedProjectId();
  const projectId = entityProjectId ?? selectedProjectId;
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const groups = useMemo(
    () =>
      groupAttachmentsByKind({
        tools,
        refs: collectToolReferences(yamlJsonObject),
        getToolkitName: getToolkitNameFromSchema,
        hiddenKinds: isMcpVisible ? [] : [PICKER_KINDS.mcp],
        hiddenToolIds: attachmentToolkitId !== undefined ? [attachmentToolkitId] : [],
      }),
    [attachmentToolkitId, getToolkitNameFromSchema, isMcpVisible, tools, yamlJsonObject],
  );

  return (
    <BasicAccordion
      data-testid="pipeline-tools-section"
      showMode="left"
      {...(sx ? { slotSx: { root: sx } } : {})}
      items={[
        {
          title: t('pipelines.toolPicker.toolsSectionTitle', 'Tools'),
          content: (
            <Box sx={contentSx}>
              {groups.length === 0 ? (
                <Typography
                  variant="bodySmall"
                  color="text.primary"
                  data-testid="pipeline-tools-section-empty"
                >
                  {t(
                    'pipelines.toolPicker.toolsSectionEmpty',
                    'Tools you select in nodes will appear here.',
                  )}
                </Typography>
              ) : (
                <Box sx={groupSx} data-testid="pipeline-tools-list">
                  {groups.flatMap((group) =>
                    group.items.map((item) => (
                      <PipelineToolCard
                        key={item.key}
                        tool={item.tool}
                        kind={item.kind}
                        usedBy={item.usedBy}
                        onSelectNode={onSelectNode}
                        projectId={projectId}
                        validationOverride={getToolValidation?.(item.tool)}
                        onMcpLoginClick={onMcpLoginClick}
                        onRemove={canRemoveUnused && item.usedBy.length === 0 ? onRemoveTool : undefined}
                      />
                    )),
                  )}
                </Box>
              )}
              {modulesSlot ? (
                <Box sx={groupSx}>
                  <Typography variant="labelSmall" sx={groupTitleSx}>
                    {t('pipelines.toolPicker.modulesTitle', 'Modules')}
                  </Typography>
                  {modulesSlot}
                </Box>
              ) : null}
            </Box>
          ),
        },
      ]}
    />
  );
});
