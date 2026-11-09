/**
 * The admin nav's header: the logo (which doubles as the collapse toggle, as it
 * does in the reference), the "{{product}} Admin" title, and the theme toggle.
 *
 * The reference's socket-connected dot is deliberately absent — `AdminNav.tsx`'s
 * header has the full reason.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BrandLogoMark } from '@/shared/ui/brand-logo';

import { focusRing } from './AdminNavChrome';

export interface AdminNavHeaderProps {
  readonly collapsed: boolean;
  readonly onToggle: () => void;
}

export function AdminNavHeader({ collapsed, onToggle }: AdminNavHeaderProps): ReactNode {
  return (
    <Box
      sx={(theme: Theme) => ({
        height: '3.75rem',
        minHeight: '3.75rem',
        padding: '0 0.75rem',
        boxSizing: 'border-box',
        borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        display: 'flex',
        alignItems: 'center',
        // No `space-between` any more: the theme toggle that used to sit on
        // the right moved to the footer (see `AdminNavFooter`). It is ~7rem
        // wide in a 13.75rem rail, which left the product name about 2rem and
        // rendered "{{product}} Admin" as "El…".
        justifyContent: collapsed ? 'center' : 'flex-start',
        gap: '0.5rem',
      })}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem', minWidth: 0 }}>
        {/*
          The logo doubles as the collapse toggle, as it does in the reference —
          but as a real <button>, so it is reachable by keyboard. The reference's
          socket dot is deliberately absent; see this file's header.
        */}
        <Box
          component="button"
          type="button"
          data-testid="admin-nav-logo-toggle"
          // Deliberately NOT the same accessible name as the edge toggle below.
          // Two controls sharing one name is ambiguous to anyone navigating by
          // name — a screen-reader user asked to "click Collapse navigation"
          // has no way to tell which is meant.
          aria-label={t('pages.admin.nav.toggle', 'Toggle navigation')}
          aria-expanded={!collapsed}
          onClick={onToggle}
          sx={(theme: Theme) => ({
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            width: '2.25rem',
            height: '2.25rem',
            padding: 0,
            border: 'none',
            appearance: 'none',
            background: 'transparent',
            borderRadius: theme.vars.shape.radiusPill,
            cursor: 'pointer',
            '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
            ...focusRing(theme),
          })}
        >
          {/*
            * The MARK, not the wordmark. `LogoIcon` is `0 0 99 20`; forcing it
            * into a 1.75rem square rendered as an illegible smudge here — the
            * same defect the main app's `SidebarHeader` had. Pack-driven
            * (ADR-0024 WP3): a served custom `assets.logoMark` renders as an
            * <img>; otherwise this is the compiled `LogoMarkIcon`.
            */}
          <BrandLogoMark style={{ width: '1.75rem', height: '1.75rem' }} />
        </Box>
        {!collapsed && (
          <Typography
            variant="headingSmall"
            // `component="span"`, deliberately. `MuiTypography`'s override maps
            // `headingSmall` to a real `<h3>`, and this nav is persistent chrome
            // on pages whose own heading is an `<h5>` — so the styled default
            // put an h3 above every h5 and axe's `heading-order` (correctly)
            // called the skipped level a violation on all ten pages. The product
            // name beside the logo is branding, not a section heading; the nav's
            // accessible name is its `aria-label`.
            component="span"
            sx={(theme: Theme) => ({
              color: theme.vars.palette.text.secondary,
              whiteSpace: 'nowrap',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
            })}
          >
            {t('pages.admin.nav.title', '{{product}} Admin')}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

