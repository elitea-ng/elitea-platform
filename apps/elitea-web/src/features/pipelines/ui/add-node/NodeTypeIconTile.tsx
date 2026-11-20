import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import { useTheme, type SxProps, type Theme } from '@mui/material/styles';

import { combineSx } from '@/shared/ui/lib/combineSx';

import { getNodeColor, getNodeIconByType } from '../../lib/flow-editor/helpers/node.helpers';

export interface NodeTypeIconTileProps {
  readonly type: string;
  readonly sx?: SxProps<Theme> | undefined;
}

const tileBaseSx = (iconBackground: string): SxProps<Theme> => ({
  display: 'flex',
  justifyContent: 'center',
  alignItems: 'center',
  flexShrink: 0,
  width: '1.75rem',
  height: '1.75rem',
  padding: '0.375rem',
  boxSizing: 'border-box',
  borderRadius: ({ vars }) => vars.shape.radiusMd,
  border: ({ vars }) => `0.0625rem solid ${vars.palette.border.lines}`,
  backgroundColor: iconBackground,
  '& svg': { width: '1rem', height: '1rem', display: 'block' },
});

/** 28px rounded icon tile in the node type's color, used in Select node type and node panel headers. */
export const NodeTypeIconTile = memo(function NodeTypeIconTile({ type, sx }: NodeTypeIconTileProps): ReactNode {
  const theme = useTheme();
  const iconBackground = getNodeColor(type, theme);

  return (
    <Box
      sx={combineSx(tileBaseSx(iconBackground), sx)}
      data-testid="pipeline-node-type-icon-tile"
    >
      {getNodeIconByType(type, theme, theme.vars.palette.icon.fill.secondary)}
    </Box>
  );
});
