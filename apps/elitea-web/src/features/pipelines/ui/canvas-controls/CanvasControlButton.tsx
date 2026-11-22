import type { ReactNode } from 'react';
import { forwardRef, memo } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';

export interface CanvasControlButtonProps {
  readonly title: string;
  readonly icon: ReactNode;
  readonly onClick: () => void;
  readonly disabled?: boolean | undefined;
  readonly isActive?: boolean | undefined;
  readonly testId?: string | undefined;
  readonly ariaPressed?: boolean | undefined;
  readonly ariaHaspopup?: 'menu' | undefined;
}

const wrapperSx: SxProps<Theme> = { display: 'inline-flex' };

function buttonSx(isActive: boolean): SxProps<Theme> {
  return (theme) => ({
    width: '1.625rem',
    height: '1.625rem',
    minWidth: '1.625rem',
    padding: 0,
    borderRadius: theme.vars.shape.radiusSm,
    color: isActive ? theme.vars.palette.primary.main : theme.vars.palette.icon.fill.secondary,
    '&:hover': {
      color: theme.vars.palette.primary.main,
    },
    '& svg': {
      width: '0.875rem',
      height: '0.875rem',
      display: 'block',
    },
  });
}

export const CanvasControlButton = memo(
  forwardRef<HTMLButtonElement, CanvasControlButtonProps>(function CanvasControlButton(
    { title, icon, onClick, disabled = false, isActive = false, testId, ariaPressed, ariaHaspopup },
    ref,
  ): ReactNode {
    return (
      <Tooltip title={title} placement="right">
        <Box component="span" sx={wrapperSx}>
          <BaseBtn
            ref={ref}
            variant={BUTTON_VARIANTS.tertiary}
            onClick={onClick}
            disabled={disabled}
            aria-label={title}
            aria-pressed={ariaPressed}
            aria-haspopup={ariaHaspopup}
            data-testid={testId}
            sx={buttonSx(isActive)}
          >
            {icon}
          </BaseBtn>
        </Box>
      </Tooltip>
    );
  }),
);
