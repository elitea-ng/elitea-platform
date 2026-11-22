import type { ReactNode } from 'react';
import { memo, useCallback } from 'react';

import { useTheme } from '@mui/material/styles';
import { MiniMap, type Node } from '@xyflow/react';

import { t } from '@/shared/i18n';

export interface CanvasMiniMapProps {
  /** Horizontal offset in pixels from the right edge (e.g. `400` when `NodeConfigDrawer` is open). */
  readonly rightOffsetPx?: number | undefined;
}

const MINIMAP_WIDTH = 200;
const MINIMAP_HEIGHT = 140;

export const CanvasMiniMap = memo(function CanvasMiniMap({ rightOffsetPx = 0 }: CanvasMiniMapProps): ReactNode {
  const theme = useTheme();
  const selectedColor = theme.vars.palette.primary.main;
  const defaultColor = theme.vars.palette.border.flowNode;

  const getNodeColor = useCallback(
    (node: Node) => (node.selected ? selectedColor : defaultColor),
    [defaultColor, selectedColor],
  );

  return (
    <MiniMap
      pannable
      position="bottom-right"
      nodeColor={getNodeColor}
      nodeStrokeWidth={0}
      nodeBorderRadius={12}
      bgColor={theme.vars.palette.background.paper}
      maskColor={theme.vars.palette.background.secondary}
      maskStrokeColor={selectedColor}
      maskStrokeWidth={2}
      ariaLabel={t('pipelines.canvasControls.minimapAriaLabel', 'Pipeline minimap')}
      style={{
        width: MINIMAP_WIDTH,
        height: MINIMAP_HEIGHT,
        right: `${rightOffsetPx / 16}rem`,
        border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        borderRadius: theme.vars.shape.radiusMd,
        overflow: 'hidden',
        transition: 'right 0.2s ease',
      }}
      data-testid="pipeline-canvas-minimap"
    />
  );
});
