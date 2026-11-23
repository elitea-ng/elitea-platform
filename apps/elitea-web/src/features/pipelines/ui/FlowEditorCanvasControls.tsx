/**
 * Canvas background, Wave U2 `CanvasControls` strip (Zoom, Fit, Lock, TB/LR Auto-arrange,
 * Minimap toggle), and persisted `CanvasMiniMap` rendered inside `<ReactFlow>`.
 */
import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import { useTheme } from '@mui/material/styles';
import { Background } from '@xyflow/react';

import { createStorage } from '@/shared/lib/storage';

import {
  CanvasControls,
  CanvasMiniMap,
  type CanvasArrangeOrientation,
} from './canvas-controls';

const MINIMAP_STORAGE_KEY = 'pipeline.flow.minimapVisible';

function readPersistedMinimapVisibility(): boolean {
  try {
    return createStorage('local').get(MINIMAP_STORAGE_KEY) === 'true';
  } catch {
    return false;
  }
}

function writePersistedMinimapVisibility(visible: boolean): void {
  try {
    createStorage('local').set(MINIMAP_STORAGE_KEY, String(visible));
  } catch {
    // Storage may be unavailable in restricted contexts; keep in-memory state.
  }
}

/** `<ReactFlow>` dotted canvas background. */
export function FlowEditorBackground(): ReactNode {
  const theme = useTheme();
  return (
    <Background
      color={theme.vars.palette.border.lines}
      bgColor={theme.vars.palette.background.secondary}
      size={1.5}
      offset={[0, 2]}
      gap={20}
    />
  );
}

export interface FlowEditorCanvasControlsProps {
  readonly isLocked: boolean;
  readonly onToggleLock: () => void;
  readonly orientation: CanvasArrangeOrientation;
  readonly onArrange: (orientation: CanvasArrangeOrientation) => void;
  readonly canArrange?: boolean | undefined;
  readonly rightOffsetPx?: number | undefined;
}

export function FlowEditorCanvasControls({
  isLocked,
  onToggleLock,
  orientation,
  onArrange,
  canArrange = true,
  rightOffsetPx = 0,
}: FlowEditorCanvasControlsProps): ReactNode {
  const [isMinimapVisible, setIsMinimapVisible] = useState<boolean>(readPersistedMinimapVisibility);

  const handleToggleMinimap = useCallback(() => {
    setIsMinimapVisible(prev => {
      const next = !prev;
      writePersistedMinimapVisibility(next);
      return next;
    });
  }, []);

  return (
    <>
      <CanvasControls
        isLocked={isLocked}
        onToggleLock={onToggleLock}
        orientation={orientation}
        onArrange={onArrange}
        canArrange={canArrange}
        isMinimapVisible={isMinimapVisible}
        onToggleMinimap={handleToggleMinimap}
      />
      {isMinimapVisible && <CanvasMiniMap rightOffsetPx={rightOffsetPx} />}
    </>
  );
}
