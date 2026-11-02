import type { ReactNode } from 'react';

import { Link, useNavigate, useRouterState } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { ResourcesIcon } from '@/shared/ui/icons/resources-icon';
import { AccountMenu, type AccountMenuItem } from '@/widgets/account-menu';

/**
 * The rail's foot: the account menu (`widgets/account-menu`, the same one the
 * desktop app has) and, when the deployment has a support assistant, the
 * "Support Bot | ?" bar under it (`SidebarBody.jsx`'s `styles.footerBlock`).
 *
 * Settings, Credentials, the Catalog and Help Center were rows here; they
 * live in the account menu now, beside the version and Sign out, as on the
 * desktop. The assistant bar stays a bar: it opens an assistant, not a page.
 */

/** `SidebarBody.jsx`'s `styles.footerBlock` height. */
const FOOTER_BAR_HEIGHT = '3.25rem';

export interface SidebarAccountProps {
  collapsed: boolean;
  /** Credentials, when the person may open them (`visibleNavSections`). */
  credentials?: boolean;
}

/** The account menu at the rail's foot, outside the scroll area. */
export function SidebarAccount({ collapsed, credentials = false }: SidebarAccountProps): ReactNode {
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });
  const navigate = useNavigate();
  const items: AccountMenuItem[] = [
    ...(credentials ? [{ value: 'credentials', label: t('widgets.sidebar.nav.credentials', 'Credentials'), url: '/credentials' }] : []),
    { value: 'catalog', label: t('widgets.sidebar.eliteaCatalog', 'Catalog'), url: '/elitea-catalog' },
    { value: 'help', label: t('widgets.sidebar.helpCenter', 'Help Center'), url: '/help-center' },
  ];
  const selected = ['/settings', '/credentials', '/elitea-catalog', '/help-center'].some((path) => pathname.startsWith(path));
  return (
    <Box sx={accountSx(collapsed)}>
      <AccountMenu items={items} go={(to) => void navigate({ to })} platform="web" collapsed={collapsed} selected={selected} />
    </Box>
  );
}

export interface SidebarFooterBarProps {
  collapsed: boolean;
  /** Provided only when the deployment has a support assistant to open (`widgets/support-assistant`'s render prop). */
  onToggleAssistant?: (() => void) | undefined;
}

/** Block 2 — the bar under the scroll area: Support Bot | "?", or the Help Center row. */
export function SidebarFooterBar({ collapsed, onToggleAssistant }: SidebarFooterBarProps): ReactNode {
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });
  const onHelp = pathname === '/help-center';

  // No assistant: nothing here. Help Center is in the account menu.
  if (onToggleAssistant === undefined) return null;

  const supportLabel = t('widgets.sidebar.supportBot', 'Support Bot');
  return (
    <Box sx={footerBlockSx}>
      <Tooltip
        title={collapsed ? supportLabel : ''}
        placement="left"
      >
        <Box
          component="button"
          type="button"
          data-testid="sidebar-support-assistant"
          aria-label={supportLabel}
          onClick={onToggleAssistant}
          sx={assistantBlockSx(collapsed)}
        >
          {collapsed ? null : (
            <Typography
              variant="labelSmall"
              component="span"
              sx={assistantLabelSx}
            >
              {supportLabel}
            </Typography>
          )}
        </Box>
      </Tooltip>
      {!collapsed && (
        <Box
          component={Link}
          to="/help-center"
          aria-label={t('widgets.sidebar.helpCenter', 'Help Center')}
          aria-current={onHelp ? 'page' : undefined}
          sx={helpIconOuterSx}
        >
          <Box sx={helpIconButtonSx(onHelp)}>
            <ResourcesIcon style={{ width: '1.25rem', height: '1.25rem' }} />
          </Box>
        </Box>
      )}
    </Box>
  );
}

/** The account row: a rule above it, the rail's inline padding. */
const accountSx = (collapsed: boolean) => (theme: Theme) => ({
  flexShrink: 0,
  paddingBlock: '0.25rem',
  paddingInline: collapsed ? '0.25rem' : '0.5rem',
  borderTop: `0.0625rem solid ${theme.vars.palette.border.sidebarDivider}`,
});

/** The bar's 1px top rule (`styles.footerBlock`'s `:before`). */
const footerRuleSx = (theme: Theme) => ({
  position: 'relative',
  flexShrink: 0,
  '::before': {
    content: '""',
    position: 'absolute',
    left: 0,
    top: 0,
    width: '100%',
    height: '0.0625rem',
    backgroundColor: theme.vars.palette.border.sidebarDivider,
  },
});

const footerBlockSx = (theme: Theme) => ({
  ...footerRuleSx(theme),
  display: 'flex',
  alignItems: 'stretch',
  height: FOOTER_BAR_HEIGHT,
});

/**
 * `styles.assistantBlock`. The label's `margin-left: 1.5rem` is not
 * decorative: `widgets/support-assistant` mounts its floating launcher
 * `bottom-left`, i.e. directly over this block's left end, and the offset is
 * what stops the word "Support Bot" from running under that avatar.
 */
const assistantBlockSx = (collapsed: boolean) => (theme: Theme) => ({
  flex: 1,
  display: 'flex',
  justifyContent: 'center',
  alignItems: 'center',
  minWidth: 0,
  appearance: 'none',
  background: 'transparent',
  border: 'none',
  borderRight: collapsed ? 'none' : `0.0625rem solid ${theme.vars.palette.border.sidebarDivider}`,
  cursor: 'pointer',
  '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
});

/**
 * `styles.assistantBlock`'s `span` rule — 12px/500 is `labelSmall`, so the
 * variant carries the type and only the colour and the offset stay here.
 */
const assistantLabelSx = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
  marginLeft: '1.5rem',
});

/** `ResourcesButton.jsx`'s non-`fullWidth` `styles.container`. */
const helpIconOuterSx = (theme: Theme) => ({
  width: FOOTER_BAR_HEIGHT,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  flexShrink: 0,
  color: theme.vars.palette.text.metrics,
  textDecoration: 'none',
});

/** `ResourcesButton.jsx`'s `styles.iconButton`. */
const helpIconButtonSx = (active: boolean) => (theme: Theme) => ({
  width: '2rem',
  height: '2rem',
  borderRadius: theme.vars.shape.radiusMd,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  background: active ? theme.vars.palette.background.button.drawerMenu.selected : 'transparent',
  '&:hover': { backgroundColor: theme.vars.palette.background.button.drawerMenu.hover },
  '&:active': { backgroundColor: theme.vars.palette.background.button.drawerMenu.selected },
});
