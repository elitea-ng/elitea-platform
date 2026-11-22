import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Slide from '@mui/material/Slide';
import type { SxProps, Theme } from '@mui/material/styles';

/** Fixed 400px width of the Wave U2 node configuration drawer (`25rem` at 16px root). */
export const NODE_CONFIG_DRAWER_WIDTH_REM = '25rem';

export interface NodeConfigDrawerProps {
  readonly open: boolean;
  readonly onExited?: (() => void) | undefined;
  readonly container?: HTMLElement | null | undefined;
  readonly children: ReactNode;
}

const rootSx: SxProps<Theme> = {
  position: 'absolute',
  top: '-0.0625rem',
  right: '-0.0625rem',
  bottom: '-0.0625rem',
  width: NODE_CONFIG_DRAWER_WIDTH_REM,
  maxWidth: '100%',
  zIndex: 110,
};

/**
 * Overlays the 400px node configuration panel on the canvas's right edge without shifting canvas controls.
 */
export const NodeConfigDrawer = memo(function NodeConfigDrawer({
  open,
  onExited,
  container,
  children,
}: NodeConfigDrawerProps): ReactNode {
  return (
    <Slide
      in={open}
      direction="left"
      {...(container !== undefined ? { container } : {})}
      {...(onExited !== undefined ? { onExited } : {})}
      mountOnEnter
      unmountOnExit
    >
      <Box sx={rootSx} data-testid="pipeline-node-panel-drawer">
        {children}
      </Box>
    </Slide>
  );
});
