/**
 * Chrome shared by the admin nav's pieces (issue #225) — the focus indicator
 * and the group divider. Its own module so `AdminNav.tsx`, `AdminNavHeader.tsx`
 * and `AdminNavFooter.tsx` share ONE definition of each rather than three that
 * can drift; a second focus treatment that quietly stopped matching the first
 * is invisible until a keyboard user hits it.
 */
import type { ReactNode } from 'react';

import Divider from '@mui/material/Divider';
import type { Theme } from '@mui/material/styles';

/**
 * A visible focus indicator, applied to every interactive element in this nav.
 *
 * MUI's default `ListItemButton` focus treatment is a translucent overlay that
 * is nearly invisible against this nav's own hover/selected tints, so keyboard
 * users could not tell where they were. `focus.ring` clears 3:1 against both
 * schemes' sidebar backgrounds (pinned in `AdminNav.test.tsx`).
 */
export function focusRing(theme: Theme): Record<string, unknown> {
  return {
    '&:focus-visible': {
      outline: `0.125rem solid ${theme.vars.palette.focus.ring}`,
      outlineOffset: '0.125rem',
    },
  };
}

export function AdminNavDivider(): ReactNode {
  return (
    <Divider
      sx={(theme: Theme) => ({
        borderColor: theme.vars.palette.border.sidebarDivider,
        marginInline: '0.75rem',
      })}
    />
  );
}

