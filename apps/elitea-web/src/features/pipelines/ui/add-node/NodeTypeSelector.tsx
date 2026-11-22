import type { ReactNode } from 'react';
import { memo, useCallback, useEffect, useMemo, useRef } from 'react';

import Box from '@mui/material/Box';
import Popover, { type PopoverOrigin, type PopoverPosition } from '@mui/material/Popover';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { PipelineNodeType } from '../../lib/flow-editor/constants/flowEditor.constants';
import { NodeTypeCard } from './NodeTypeCard';
import {
  getAddableNodeTypes,
  getNodeTypeSelectorLayout,
  type NodeTypeSelectorLayout,
} from './nodeTypeSelector.helpers';

const DEFAULT_ANCHOR_ORIGIN: PopoverOrigin = { vertical: 'bottom', horizontal: 'right' };
const DEFAULT_TRANSFORM_ORIGIN: PopoverOrigin = { vertical: 'top', horizontal: 'right' };

const toRem = (px: number | null): string | undefined => (px === null ? undefined : `${px / 16}rem`);

export interface NodeTypeSelectorProps {
  readonly open: boolean;
  readonly anchorEl?: HTMLElement | null | undefined;
  readonly anchorPosition?: PopoverPosition | undefined;
  readonly boundaryEl?: HTMLElement | null | undefined;
  readonly availableTypes?: readonly PipelineNodeType[] | undefined;
  readonly onClose?: (() => void) | undefined;
  readonly onSelect?: ((type: PipelineNodeType) => void) | undefined;
  readonly anchorOrigin?: PopoverOrigin | undefined;
  readonly transformOrigin?: PopoverOrigin | undefined;
}

const buildPaperSx = (layout: NodeTypeSelectorLayout, isCentered: boolean): SxProps<Theme> => ({
  display: 'flex',
  flexDirection: 'column',
  marginTop: isCentered ? 0 : '0.5rem',
  padding: '1rem 1rem 1.25rem',
  boxSizing: 'border-box',
  maxWidth: toRem(layout.maxWidth),
  maxHeight: toRem(layout.maxHeight),
  borderRadius: ({ vars }) => vars.shape.radiusLg,
  border: ({ vars }) => `0.0625rem solid ${vars.palette.border.lines}`,
  backgroundColor: ({ vars }) => vars.palette.background.secondary,
  boxShadow: ({ vars }) => vars.palette.boxShadow.default,
});

const titleSx: SxProps<Theme> = {
  display: 'block',
  flexShrink: 0,
  marginBottom: '1rem',
};

const buildGridSx = (columns: 1 | 2, rowsCount: number): SxProps<Theme> => ({
  display: 'grid',
  gridTemplateColumns: `repeat(${columns}, minmax(0, 16.125rem))`,
  gridTemplateRows: `repeat(${rowsCount}, auto)`,
  gridAutoFlow: 'column',
  gap: '0.5rem',
  minHeight: 0,
  overflowY: 'auto',
});

export const NodeTypeSelector = memo(function NodeTypeSelector({
  open,
  anchorEl,
  anchorPosition,
  boundaryEl,
  availableTypes,
  onClose,
  onSelect,
  anchorOrigin = DEFAULT_ANCHOR_ORIGIN,
  transformOrigin = DEFAULT_TRANSFORM_ORIGIN,
}: NodeTypeSelectorProps): ReactNode {
  const nodeTypes = useMemo(() => {
    const addableTypes = getAddableNodeTypes();
    return availableTypes ? addableTypes.filter((type) => availableTypes.includes(type)) : addableTypes;
  }, [availableTypes]);

  const openLayoutRef = useRef<NodeTypeSelectorLayout | null>(null);

  // Measured when the selector opens so it fits the canvas it is shown over. While closing,
  // the last open layout is kept so the popover does not jump to full size during its exit transition.
  const layout = useMemo<NodeTypeSelectorLayout>(() => {
    if (!open && openLayoutRef.current) return openLayoutRef.current;

    return getNodeTypeSelectorLayout({
      boundaryRect: open ? boundaryEl?.getBoundingClientRect() : null,
      anchorRect: open ? anchorEl?.getBoundingClientRect() : null,
      isCentered: transformOrigin.vertical === 'center',
    });
  }, [anchorEl, boundaryEl, open, transformOrigin.vertical]);

  useEffect(() => {
    if (open) openLayoutRef.current = layout;
  }, [layout, open]);

  const handleSelect = useCallback(
    (type: PipelineNodeType) => {
      onSelect?.(type);
      onClose?.();
    },
    [onClose, onSelect],
  );

  const isCentered = transformOrigin.vertical === 'center';
  const rowsCount = Math.max(1, Math.ceil(nodeTypes.length / layout.columns));

  const paperDataAttrs = useMemo<Record<string, unknown>>(
    () => ({
      'data-testid': 'pipeline-node-type-selector',
      'data-columns': layout.columns,
    }),
    [layout.columns],
  );

  return (
    <Popover
      open={open}
      anchorEl={anchorEl ?? null}
      anchorReference={anchorPosition ? 'anchorPosition' : 'anchorEl'}
      {...(anchorPosition ? { anchorPosition } : {})}
      {...(onClose ? { onClose } : {})}
      anchorOrigin={anchorOrigin}
      transformOrigin={transformOrigin}
      slotProps={{
        paper: {
          id: 'pipeline-add-node-menu',
          role: 'menu',
          sx: buildPaperSx(layout, isCentered),
          ...paperDataAttrs,
        },
      }}
    >
      <Typography
        variant="headingSmall"
        color="text.secondary"
        sx={titleSx}
      >
        {t('features.pipelines.addNode.selectNodeType', 'Select node type')}
      </Typography>
      <Box sx={buildGridSx(layout.columns, rowsCount)}>
        {nodeTypes.map((type) => (
          <NodeTypeCard
            key={type}
            type={type}
            onSelect={handleSelect}
          />
        ))}
      </Box>
    </Popover>
  );
});
