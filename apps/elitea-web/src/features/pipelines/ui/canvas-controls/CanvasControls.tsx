import type { ReactNode } from 'react';
import { memo, useCallback, useRef, useState } from 'react';

import LockOpenOutlinedIcon from '@mui/icons-material/LockOpenOutlined';
import MapOutlinedIcon from '@mui/icons-material/MapOutlined';
import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { Panel, useReactFlow } from '@xyflow/react';

import { t } from '@/shared/i18n';
import { FullScreenIcon } from '@/shared/ui/icons/full-screen-icon';
import { LockIcon } from '@/shared/ui/icons/lock-icon';
import { MinusIcon } from '@/shared/ui/icons/minus-icon';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';
import { PolylineOutlineIcon } from '@/shared/ui/icons/polyline-outline-icon';

import { CanvasArrangeMenu, type CanvasArrangeOrientation } from './CanvasArrangeMenu';
import { CanvasControlButton } from './CanvasControlButton';

const ZOOM_DURATION_MS = 150;

export interface CanvasControlsProps {
  readonly isLocked: boolean;
  readonly onToggleLock: () => void;
  readonly orientation: CanvasArrangeOrientation;
  readonly onArrange: (orientation: CanvasArrangeOrientation) => void;
  readonly canArrange?: boolean | undefined;
  readonly isMinimapVisible: boolean;
  readonly onToggleMinimap: () => void;
}

const rootSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  padding: '0.125rem',
  borderRadius: theme.vars.shape.radiusSm,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  backgroundColor: theme.vars.palette.background.paper,
});

/**
 * Standalone Wave U2 canvas control strip: Zoom in/out, Fit view, Lock/Unlock,
 * Auto-arrange direction (TB/LR), and Minimap toggle.
 */
export const CanvasControls = memo(function CanvasControls({
  isLocked,
  onToggleLock,
  orientation,
  onArrange,
  canArrange = true,
  isMinimapVisible,
  onToggleMinimap,
}: CanvasControlsProps): ReactNode {
  const { zoomIn, zoomOut, fitView } = useReactFlow();
  const arrangeButtonRef = useRef<HTMLButtonElement | null>(null);
  const [arrangeAnchorEl, setArrangeAnchorEl] = useState<HTMLElement | null>(null);

  const handleZoomIn = useCallback(() => {
    void zoomIn({ duration: ZOOM_DURATION_MS });
  }, [zoomIn]);

  const handleZoomOut = useCallback(() => {
    void zoomOut({ duration: ZOOM_DURATION_MS });
  }, [zoomOut]);

  const handleFitView = useCallback(() => {
    void fitView({ duration: ZOOM_DURATION_MS });
  }, [fitView]);

  const handleOpenArrange = useCallback(() => setArrangeAnchorEl(arrangeButtonRef.current), []);
  const handleCloseArrange = useCallback(() => setArrangeAnchorEl(null), []);

  return (
    <Panel position="bottom-left">
      <Box sx={rootSx} data-testid="pipeline-canvas-controls">
        <CanvasControlButton
          title={t('pipelines.canvasControls.zoomIn', 'Zoom in')}
          icon={<PlusIcon />}
          onClick={handleZoomIn}
          testId="pipeline-canvas-zoom-in"
        />
        <CanvasControlButton
          title={t('pipelines.canvasControls.zoomOut', 'Zoom out')}
          icon={<MinusIcon />}
          onClick={handleZoomOut}
          testId="pipeline-canvas-zoom-out"
        />
        <CanvasControlButton
          title={t('pipelines.canvasControls.fitView', 'Fit view')}
          icon={<FullScreenIcon />}
          onClick={handleFitView}
          testId="pipeline-canvas-fit-view"
        />
        <CanvasControlButton
          title={
            isLocked
              ? t('pipelines.canvasControls.unlockCanvas', 'Unlock canvas')
              : t('pipelines.canvasControls.lockCanvas', 'Lock canvas')
          }
          icon={isLocked ? <LockIcon /> : <LockOpenOutlinedIcon />}
          onClick={onToggleLock}
          isActive={isLocked}
          ariaPressed={isLocked}
          testId="pipeline-canvas-lock"
        />
        <CanvasControlButton
          ref={arrangeButtonRef}
          title={t('pipelines.canvasControls.autoArrange', 'Auto-arrange')}
          icon={<PolylineOutlineIcon />}
          onClick={handleOpenArrange}
          disabled={!canArrange}
          isActive={Boolean(arrangeAnchorEl)}
          ariaHaspopup="menu"
          testId="pipeline-canvas-arrange"
        />
        <CanvasControlButton
          title={
            isMinimapVisible
              ? t('pipelines.canvasControls.hideMinimap', 'Hide minimap')
              : t('pipelines.canvasControls.showMinimap', 'Show minimap')
          }
          icon={<MapOutlinedIcon />}
          onClick={onToggleMinimap}
          isActive={isMinimapVisible}
          ariaPressed={isMinimapVisible}
          testId="pipeline-canvas-minimap-toggle"
        />
      </Box>
      <CanvasArrangeMenu
        anchorEl={arrangeAnchorEl}
        orientation={orientation}
        onArrange={onArrange}
        onClose={handleCloseArrange}
      />
    </Panel>
  );
});
