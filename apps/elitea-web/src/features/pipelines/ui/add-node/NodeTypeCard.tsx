import type { KeyboardEvent, ReactNode } from 'react';
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import type { PipelineNodeType } from '../../lib/flow-editor/constants/flowEditor.constants';
import { PipelineNodeDescriptions } from './nodeTypeDescriptions';
import { NodeTypeIconTile } from './NodeTypeIconTile';
import { toNodeTypeTestIdSegment } from './nodeTypeSelector.helpers';

export interface NodeTypeCardProps {
  readonly type: PipelineNodeType;
  readonly onSelect?: ((type: PipelineNodeType) => void) | undefined;
}

const cardRootSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'flex-start',
  gap: '0.5rem',
  minHeight: '4.5rem',
  padding: '0.5rem 1rem 0.5rem 0.75rem',
  boxSizing: 'border-box',
  borderRadius: ({ vars }) => vars.shape.radiusLg,
  cursor: 'pointer',
  backgroundColor: ({ vars }) => vars.palette.background.card.default,
  '&:hover': {
    backgroundColor: ({ vars }) => vars.palette.background.card.hover,
  },
  '&:focus-visible': {
    backgroundColor: ({ vars }) => vars.palette.background.card.hover,
    outline: ({ vars }) => `0.125rem solid ${vars.palette.focus.ring}`,
    outlineOffset: '-0.125rem',
  },
};

const textContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  minWidth: 0,
};

const descriptionSx: SxProps<Theme> = {
  display: '-webkit-box',
  WebkitLineClamp: 2,
  WebkitBoxOrient: 'vertical',
  overflow: 'hidden',
};

export const NodeTypeCard = memo(function NodeTypeCard({ type, onSelect }: NodeTypeCardProps): ReactNode {
  const displayName = FlowEditorConstants.PipelineNodeDisplayNames[type];
  const description = PipelineNodeDescriptions[type] ?? '';

  const handleClick = useCallback(() => {
    onSelect?.(type);
  }, [onSelect, type]);

  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        onSelect?.(type);
      }
    },
    [onSelect, type],
  );

  return (
    <Box
      role="menuitem"
      tabIndex={0}
      aria-label={displayName}
      data-testid={`pipeline-add-node-menu-item-${toNodeTypeTestIdSegment(type)}`}
      onClick={handleClick}
      onKeyDown={handleKeyDown}
      sx={cardRootSx}
    >
      <NodeTypeIconTile type={type} />
      <Box sx={textContainerSx}>
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          noWrap
        >
          {displayName}
        </Typography>
        <Typography
          variant="bodySmall"
          color="text.primary"
          sx={descriptionSx}
        >
          {description}
        </Typography>
      </Box>
    </Box>
  );
});
