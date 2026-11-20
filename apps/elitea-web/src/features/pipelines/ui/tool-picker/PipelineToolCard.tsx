import { memo, useCallback, useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';
import { TypographyWithConditionalTooltip } from '@/shared/ui/TypographyWithConditionalTooltip';

import { getToolkitDisplayName } from '../../lib/tool-picker/pipelineToolUsage.helpers';
import {
  PICKER_KINDS,
  PIPELINE_TOOL_CARD_ACTION_CLASS,
} from '../../lib/tool-picker/toolPicker.constants';
import type {
  PickerKind,
  PickerToolEntry,
  ToolValidationOverride,
  ToolkitListRow,
} from '../../lib/tool-picker/toolPicker.types';
import { PickerEntityIcon } from './PickerEntityIcon';
import { PickerMcpStatus } from './PickerMcpStatus';
import { PipelineToolCardActions } from './PipelineToolCardActions';
import { PipelineToolValidationBanner } from './PipelineToolValidationBanner';
import { PipelineToolVersionLabel } from './PipelineToolVersionLabel';

export interface PipelineToolCardProps {
  readonly tool: PickerToolEntry;
  readonly kind: PickerKind;
  readonly usedBy?: readonly string[] | undefined;
  readonly onSelectNode?: ((nodeId: string) => void) | undefined;
  readonly onRemove?: ((tool: PickerToolEntry) => void) | undefined;
  readonly projectId?: string | undefined;
  readonly validationOverride?: ToolValidationOverride | null | undefined;
  readonly onMcpLoginClick?: ((mcp: PickerToolEntry | ToolkitListRow) => void) | undefined;
  readonly createCredentialHref?: string | undefined;
}

const headerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(2),
  minHeight: '3.75rem',
  paddingBlock: theme.spacing(1),
  paddingInline: theme.spacing(2),
  boxSizing: 'border-box',
});

const contentSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  justifyContent: 'center',
  flex: '1 1 0',
  minWidth: 0,
};

const ellipsisSx: SxProps<Theme> = {
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  minWidth: 0,
};

const toolsToggleSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  alignSelf: 'flex-start',
  gap: theme.spacing(0.5),
  cursor: 'pointer',
  color: theme.vars.palette.text.primary,
  outline: 'none',
  '&:hover, &:focus-visible': {
    color: theme.vars.palette.text.createButton,
  },
});

const usageSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexWrap: 'wrap',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  paddingTop: 0,
  paddingRight: theme.spacing(2),
  paddingBottom: theme.spacing(1),
  paddingLeft: '4.125rem',
});

const usageTextSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
});

const nodeChipSx: SxProps<Theme> = (theme: Theme) => ({
  height: '1.25rem',
  maxWidth: '100%',
  backgroundColor: theme.vars.palette.background.tagChip.default,
});

const toolsListSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexWrap: 'wrap',
  gap: theme.spacing(0.75),
  paddingBlock: theme.spacing(1),
  paddingInline: theme.spacing(2),
  borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});

const toolChipSx: SxProps<Theme> = (theme: Theme) => ({
  maxWidth: '100%',
  backgroundColor: theme.vars.palette.background.tagChip.default,
});

function buildRootSx(showTools: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    borderRadius: theme.vars.shape.radiusMd,
    border: `0.0625rem solid ${theme.vars.palette.border.cardsOutlines}`,
    backgroundColor: theme.vars.palette.background.paper,
    overflow: 'hidden',
    [`& .${PIPELINE_TOOL_CARD_ACTION_CLASS}`]: { display: 'none' },
    [`&:hover .${PIPELINE_TOOL_CARD_ACTION_CLASS}, &:focus-within .${PIPELINE_TOOL_CARD_ACTION_CLASS}`]: {
      display: 'flex',
    },
    ...(showTools
      ? {}
      : {
          '&:hover': {
            borderColor: theme.vars.palette.border.lines,
            backgroundColor: theme.vars.palette.background.toolCard.hover,
          },
        }),
  });
}

function buildToggleIconSx(showTools: boolean): SxProps<Theme> {
  return {
    width: '1rem',
    height: '1rem',
    transform: showTools ? 'rotate(90deg)' : 'none',
    transition: 'transform 0.2s',
  };
}

function formatToolLabel(toolName: string): string {
  return (toolName.charAt(0).toUpperCase() + toolName.slice(1)).replaceAll('_', ' ');
}

function getEntityLabel(kind: PickerKind): string {
  if (kind === PICKER_KINDS.mcp) return t('pipelines.toolPicker.entityMcp', 'MCP');
  if (kind === PICKER_KINDS.agent) return t('pipelines.toolPicker.entityAgent', 'agent');
  if (kind === PICKER_KINDS.pipeline) return t('pipelines.toolPicker.entityPipeline', 'pipeline');
  return t('pipelines.toolPicker.entityToolkit', 'toolkit');
}

function PipelineToolCardSubtitle({
  isApplication,
  tool,
  projectId,
  exposedToolsCount,
  showTools,
  onToggleTools,
}: {
  readonly isApplication: boolean;
  readonly tool: PickerToolEntry;
  readonly projectId: string | undefined;
  readonly exposedToolsCount: number;
  readonly showTools: boolean;
  readonly onToggleTools: () => void;
}): ReactNode {
  if (isApplication) {
    return <PipelineToolVersionLabel tool={tool} projectId={projectId} />;
  }
  if (exposedToolsCount > 0) {
    return (
      <ButtonBase
        type="button"
        aria-expanded={showTools}
        onClick={onToggleTools}
        sx={toolsToggleSx}
        data-testid="pipeline-tool-card-show-tools"
      >
        <Typography variant="bodySmall" color="inherit">
          {showTools
            ? t('pipelines.toolPicker.hideTools', 'Hide tools')
            : t('pipelines.toolPicker.showTools', 'Show tools')}
        </Typography>
        <Box component={ArrowRightIcon} sx={buildToggleIconSx(showTools)} />
      </ButtonBase>
    );
  }
  if (tool.description) {
    return (
      <Typography variant="bodySmall" color="text.primary" noWrap>
        {tool.description}
      </Typography>
    );
  }
  return null;
}

function PipelineToolCardUsage({
  usedBy,
  onSelectNode,
}: {
  readonly usedBy: readonly string[];
  readonly onSelectNode: ((nodeId: string) => void) | undefined;
}): ReactNode {
  if (usedBy.length === 0) {
    return (
      <Box sx={usageSx}>
        <Typography variant="labelSmall" sx={usageTextSx} data-testid="pipeline-tool-card-not-used">
          {t('pipelines.toolPicker.notUsedByAnyNode', 'Not used by any node')}
        </Typography>
      </Box>
    );
  }
  return (
    <Box sx={usageSx}>
      <Typography variant="labelSmall" sx={usageTextSx}>
        {t('pipelines.toolPicker.usedBy', 'Used by')}
      </Typography>
      {usedBy.map((nodeId) => (
        <Chip
          key={nodeId}
          size="small"
          label={
            <Typography variant="labelSmall" sx={usageTextSx}>
              {nodeId}
            </Typography>
          }
          sx={nodeChipSx}
          onClick={onSelectNode ? () => onSelectNode(nodeId) : undefined}
          data-testid="pipeline-tool-card-used-by"
        />
      ))}
    </Box>
  );
}

/**
 * Read-only card of a pipeline attachment: icon, name, version or expandable "Show tools",
 * "Used by" node chips, hover actions, MCP status, and EL-7516 misconfigured-toolkit warning banner.
 */
export const PipelineToolCard = memo(function PipelineToolCard({
  tool,
  kind,
  usedBy = [],
  onSelectNode,
  onRemove,
  projectId,
  validationOverride,
  onMcpLoginClick,
  createCredentialHref,
}: PipelineToolCardProps): ReactNode {
  const [showTools, setShowTools] = useState(false);
  const toggleTools = useCallback(() => setShowTools((prev) => !prev), []);

  const isApplication = kind === PICKER_KINDS.agent || kind === PICKER_KINDS.pipeline;
  const name = isApplication ? tool.name ?? '' : getToolkitDisplayName(tool);

  const exposedTools = useMemo(
    () =>
      (tool.settings?.selected_tools ?? [])
        .map((item) => (typeof item === 'string' ? item : item?.name))
        .filter((item): item is string => typeof item === 'string' && item.length > 0),
    [tool.settings?.selected_tools],
  );

  return (
    <>
      <Box sx={buildRootSx(showTools)} data-testid="pipeline-tool-card">
        <Box sx={headerSx}>
          <PickerEntityIcon
            source={tool}
            mcp={kind === PICKER_KINDS.mcp}
            isApplication={isApplication}
            isPipeline={kind === PICKER_KINDS.pipeline}
            large
          />
          <Box sx={contentSx}>
            <TypographyWithConditionalTooltip
              title={name}
              placement="right"
              variant="bodyMedium"
              color="text.secondary"
              sx={ellipsisSx}
              data-testid="pipeline-tool-card-name"
            >
              {name}
            </TypographyWithConditionalTooltip>
            <PipelineToolCardSubtitle
              isApplication={isApplication}
              tool={tool}
              projectId={projectId}
              exposedToolsCount={exposedTools.length}
              showTools={showTools}
              onToggleTools={toggleTools}
            />
          </Box>
          <PipelineToolCardActions
            tool={tool}
            entityLabel={getEntityLabel(kind)}
            onRemove={onRemove}
          />
          {kind === PICKER_KINDS.mcp ? (
            <PickerMcpStatus mcp={tool} onLoginClick={onMcpLoginClick} />
          ) : null}
        </Box>

        <PipelineToolCardUsage usedBy={usedBy} onSelectNode={onSelectNode} />

        {showTools ? (
          <Box sx={toolsListSx} data-testid="pipeline-tool-card-tools">
            {exposedTools.map((toolName) => (
              <Chip
                key={toolName}
                size="small"
                label={formatToolLabel(toolName)}
                sx={toolChipSx}
              />
            ))}
          </Box>
        ) : null}
      </Box>
      <PipelineToolValidationBanner
        tool={tool}
        projectId={projectId}
        validationOverride={validationOverride}
        createCredentialHref={createCredentialHref}
      />
    </>
  );
});
