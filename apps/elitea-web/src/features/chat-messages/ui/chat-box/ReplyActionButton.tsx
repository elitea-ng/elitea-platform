/**
 * One icon in the action row under an assistant reply (`ApplicationAnswerActions`)
 * or a question (`UserMessageActions`):
 * a 28px hit target holding a 16px outlined icon in the quiet `text.metrics`
 * colour, the sidebar rows' hover background, and a tooltip naming it — the
 * composer's `ComposerIconButton` at the transcript's smaller icon size.
 *
 * The tooltip's child is a `span`: a disabled button fires no events, and the
 * tooltip of a disabled control is what says why it is disabled.
 * `describeChild` keeps the tooltip from naming that span too, so the button
 * is the only element carrying the label.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
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
    <Tooltip title={tooltip || label} placement="top" describeChild>
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

/** Marks an action row so the message around it can reveal it on hover (`replyActionsRevealSx`). */
const REPLY_ACTIONS_ATTR = 'data-reply-actions';

/** For the message's own box: hovering anywhere on the message shows its row. */
export const replyActionsRevealSx = { [`&:hover [${REPLY_ACTIONS_ATTR}]`]: { opacity: 1 } } as const;

/**
 * The quiet row the action buttons sit in, under a reply or a question. When
 * not `shown` it is faded by opacity, not hidden, so Tab still reaches every
 * control: it fades in on the message's hover (`replyActionsRevealSx`), when
 * focus enters it, and is always shown on a device with no hover.
 */
export function ReplyActionsRow({ shown, className, children }: {
  readonly shown: boolean;
  readonly className?: string | undefined;
  readonly children: ReactNode;
}): ReactNode {
  return (
    <Box
      {...{ [REPLY_ACTIONS_ATTR]: '' }}
      className={className}
      sx={{
        display: 'flex',
        alignItems: 'center',
        gap: 0.5,
        // The message's text starts 1rem in; a 28px target around a 16px icon
        // puts the first icon on that edge.
        paddingLeft: '0.625rem',
        marginTop: '-0.25rem',
        opacity: shown ? 1 : 0,
        transition: 'opacity 120ms',
        '&:focus-within': { opacity: 1 },
        '@media (hover: none)': { opacity: 1 },
      }}
    >
      {children}
    </Box>
  );
}
