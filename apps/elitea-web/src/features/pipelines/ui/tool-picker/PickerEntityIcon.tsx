import { memo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { ApplicationsIcon } from '@/shared/ui/icons/applications-icon';
import { FlowIcon } from '@/shared/ui/icons/flow-icon';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import { ToolkitTypeIcon } from '@/shared/ui/ToolkitTypeIcon';

import { isApplicationTool } from '../../lib/tool-picker/pipelineToolUsage.helpers';
import type { ApplicationListRow, PickerToolEntry, ToolkitListRow } from '../../lib/tool-picker/toolPicker.types';

export interface PickerEntityIconProps {
  readonly source?: PickerToolEntry | ApplicationListRow | ToolkitListRow | null | undefined;
  readonly isPipeline?: boolean | undefined;
  readonly mcp?: boolean | undefined;
  readonly isApplication?: boolean | undefined;
  readonly large?: boolean | undefined;
}

const smallWrapperSx: SxProps<Theme> = {
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  width: '1rem',
  height: '1rem',
  minWidth: '1rem',
  flexShrink: 0,
};

const largeWrapperSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  width: '2.125rem',
  height: '2.125rem',
  minWidth: '2.125rem',
  flexShrink: 0,
  borderRadius: theme.vars.shape.radiusPill,
  backgroundColor: theme.vars.palette.background.tagChip.default,
  color: theme.vars.palette.icon.fill.default,
});

const smallGlyphSx: SxProps<Theme> = { width: '1rem', height: '1rem' };
const largeGlyphSx: SxProps<Theme> = { width: '1.25rem', height: '1.25rem' };

function isPipelineEntity(
  source: PickerEntityIconProps['source'],
  isPipeline: boolean,
): boolean {
  if (isPipeline || !source) return isPipeline;
  if ('agent_type' in source && source.agent_type === 'pipeline') return true;
  if ('version_details' in source && source.version_details?.agent_type === 'pipeline') return true;
  return false;
}

function resolveEntityGlyph(
  source: PickerEntityIconProps['source'],
  isApp: boolean,
  isPipeline: boolean,
  mcp: boolean,
  glyphSx: SxProps<Theme>,
): ReactNode {
  if (isApp) {
    return isPipeline ? (
      <Box component={FlowIcon} sx={glyphSx} />
    ) : (
      <Box component={ApplicationsIcon} sx={glyphSx} />
    );
  }
  const toolkitType = source && 'type' in source ? source.type : undefined;
  if (mcp && (!toolkitType || toolkitType === 'mcp')) {
    return <Box component={McpIcon} sx={glyphSx} />;
  }
  return <ToolkitTypeIcon type={toolkitType} sx={glyphSx} />;
}

/** Icon of a toolkit, MCP, agent or pipeline, from a list row or an attached tool. */
export const PickerEntityIcon = memo(function PickerEntityIcon({
  source,
  isPipeline = false,
  mcp = false,
  isApplication,
  large = false,
}: PickerEntityIconProps): ReactNode {
  const resolvedIsApp = isApplication ?? isApplicationTool(source);
  const resolvedIsPipeline = isPipelineEntity(source, isPipeline);
  const glyphSx = large ? largeGlyphSx : smallGlyphSx;

  return (
    <Box sx={large ? largeWrapperSx : smallWrapperSx}>
      {resolveEntityGlyph(source, resolvedIsApp, resolvedIsPipeline, mcp, glyphSx)}
    </Box>
  );
});
