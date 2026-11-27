import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { getInitials } from '@/shared/lib/string';

export interface MonogramProps {
  /** The name the initials come from. */
  readonly name: string;
  /** Edge length, CSS. */
  readonly size: string;
  /** A Typography variant for the initials: small for a 2rem circle, larger for a card's avatar. */
  readonly variant?: 'labelSmall' | 'headingSmall';
}

/**
 * The brand-tinted initials of an agent that has no avatar of its own (issues
 * 3133 and 3134): the brand's soft accent wash under the strong text colour,
 * so a brand pack re-tints it and the initials keep their contrast. Never a
 * placeholder glyph or a broken image. Decorative: whatever sits next to it
 * carries the name.
 */
export function Monogram({ name, size, variant = 'headingSmall' }: MonogramProps): ReactNode {
  return (
    <Box
      component="span"
      aria-hidden
      data-testid="monogram"
      sx={monogramSx(size, variant)}
    >
      {getInitials(name)}
    </Box>
  );
}

function monogramSx(size: string, variant: NonNullable<MonogramProps['variant']>): SxProps<Theme> {
  return (theme: Theme) => ({
    ...theme.typography[variant],
    flexShrink: 0,
    width: size,
    height: size,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: theme.vars.shape.radiusPill,
    // The brand's soft accent wash (brand guide v3 `selected`, the primary at 10% on Paper and 22% on Ink), under the
    // strong text role: the primary itself on that wash is under 3:1 on Ink.
    background: theme.vars.palette.background.conversation.selected,
    color: theme.vars.palette.text.secondary,
    userSelect: 'none',
  });
}
