import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import type { SystemStyleObject } from '@mui/system';

import { toneColor, type StatusTone } from './tone';

export interface StatusDotProps {
  /** The state the dot stands for; its colour is the tone's role. */
  readonly tone: StatusTone;
  readonly 'data-testid'?: string | undefined;
}

/**
 * A dot in the colour of a status (ADR-0062 decision 4, the status family).
 * Ported from `apps/elitea-ui/src/[fsd]/shared/ui/status-dot/StatusDot.jsx`:
 * an `8px` circle (the theme's pill radius). The legacy dot took a palette path (`'success.main'`); this
 * one takes a tone, so a screen names the state, not the colour.
 *
 * The dot is decoration: the word next to it carries the meaning, so it is
 * hidden from screen readers.
 */
export function StatusDot({ tone, 'data-testid': dataTestId }: StatusDotProps): ReactNode {
  return (
    <Box
      component="span"
      aria-hidden
      data-testid={dataTestId}
      data-tone={tone}
      sx={[dotSx, (theme: Theme) => ({ backgroundColor: toneColor(theme, tone), borderRadius: theme.vars.shape.radiusPill })]}
    />
  );
}

const dotSx: SystemStyleObject<Theme> = {
  display: 'inline-block',
  flexShrink: 0,
  width: '0.5rem',
  height: '0.5rem',
};
