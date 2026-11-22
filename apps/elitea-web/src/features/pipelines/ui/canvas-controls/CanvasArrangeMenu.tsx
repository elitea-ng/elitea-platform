import type { ReactNode } from 'react';
import { memo, useCallback } from 'react';

import ListItemIcon from '@mui/material/ListItemIcon';
import ListItemText from '@mui/material/ListItemText';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { CheckedIcon } from '@/shared/ui/icons/checked-icon';

export type CanvasArrangeOrientation = 'TB' | 'LR';

export interface CanvasArrangeMenuProps {
  readonly anchorEl: HTMLElement | null;
  readonly orientation: CanvasArrangeOrientation;
  readonly onArrange: (orientation: CanvasArrangeOrientation) => void;
  readonly onClose: () => void;
}

const paperSx: SxProps<Theme> = { marginLeft: '0.5rem' };

const checkSlotSx: SxProps<Theme> = (theme) => ({
  justifyContent: 'flex-end',
  minWidth: '1.5rem',
  marginLeft: '1rem',
  color: theme.vars.palette.icon.fill.secondary,
});

const checkIconStyle = { width: '0.75rem', height: '0.75rem' };

export const CanvasArrangeMenu = memo(function CanvasArrangeMenu({
  anchorEl,
  orientation,
  onArrange,
  onClose,
}: CanvasArrangeMenuProps): ReactNode {
  const options: readonly { readonly orientation: CanvasArrangeOrientation; readonly label: string; readonly testId: string }[] = [
    {
      orientation: 'TB',
      label: t('pipelines.canvasControls.arrangeTb', 'Arrange top to bottom'),
      testId: 'pipeline-canvas-arrange-tb',
    },
    {
      orientation: 'LR',
      label: t('pipelines.canvasControls.arrangeLr', 'Arrange left to right'),
      testId: 'pipeline-canvas-arrange-lr',
    },
  ];

  const handleSelect = useCallback(
    (nextOrientation: CanvasArrangeOrientation) => {
      onClose();
      onArrange(nextOrientation);
    },
    [onArrange, onClose],
  );

  return (
    <Menu
      anchorEl={anchorEl}
      open={Boolean(anchorEl)}
      onClose={onClose}
      anchorOrigin={{ vertical: 'bottom', horizontal: 'right' }}
      transformOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      slotProps={{ paper: { sx: paperSx } }}
      data-testid="pipeline-canvas-arrange-menu"
    >
      {options.map((option) => {
        const isCurrent = option.orientation === orientation;
        return (
          <MenuItem
            key={option.orientation}
            selected={isCurrent}
            onClick={() => handleSelect(option.orientation)}
            data-testid={option.testId}
            data-current={isCurrent ? 'true' : undefined}
          >
            <ListItemText slotProps={{ primary: { variant: 'bodyMedium' } }}>{option.label}</ListItemText>
            <ListItemIcon sx={checkSlotSx}>
              {isCurrent ? <CheckedIcon style={checkIconStyle} /> : null}
            </ListItemIcon>
          </MenuItem>
        );
      })}
    </Menu>
  );
});
