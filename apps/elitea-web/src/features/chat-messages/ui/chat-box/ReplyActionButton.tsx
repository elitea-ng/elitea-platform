/**
 * One icon in the action row under an assistant reply (`ApplicationAnswerActions`):
 * a 28px hit target holding a 16px outlined icon in the quiet `text.metrics`
 * colour, the sidebar rows' hover background, and a tooltip naming it — the
 * composer's `ComposerIconButton` at the transcript's smaller icon size.
 *
 * The tooltip's child is a `span`: a disabled button fires no events, and the
 * tooltip of a disabled control is what says why it is disabled.
 */
import type { ReactNode } from 'react';

import IconButton, { type IconButtonProps } from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

export interface ReplyActionButtonProps extends Omit<IconButtonProps, 'size' | 'children' | 'title' | 'color'> {
  /** The accessible name; also the tooltip unless `tooltip` says more. */
  readonly label: string;
  readonly tooltip?: string | undefined;
  readonly children: ReactNode;
  /** A toggle that is on (the caller's own vote): drawn in the accent colour. */
  readonly pressed?: boolean | undefined;
}

function replyActionSx(theme: Theme, pressed = false) {
  return {
    width: '1.75rem',
    height: '1.75rem',
    padding: 0,
    borderRadius: theme.vars.shape.radiusSm,
    color: pressed ? theme.vars.palette.primary.main : theme.vars.palette.text.metrics,
    '& svg': { width: '1rem', height: '1rem' },
    '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
  } as const;
}

export function ReplyActionButton({ label, tooltip, children, pressed, ...props }: ReplyActionButtonProps): ReactNode {
  return (
    <Tooltip title={tooltip || label} placement="top">
      <span>
        <IconButton
          {...props}
          aria-label={label}
          {...(pressed === undefined ? {} : { 'aria-pressed': pressed })}
          sx={(theme: Theme) => replyActionSx(theme, pressed === true)}
        >
          {children}
        </IconButton>
      </span>
    </Tooltip>
  );
}
