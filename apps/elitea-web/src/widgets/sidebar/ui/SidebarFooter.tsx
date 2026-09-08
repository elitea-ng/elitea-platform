import type { ReactNode } from 'react';

import { Link, useRouterState } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { GearIcon } from '@/shared/ui/icons/gear-icon';
import { ResourcesIcon } from '@/shared/ui/icons/resources-icon';
import agentHubIconUrl from '@/shared/ui/icons/svg/agent-hub-icon.svg?url';
import { t } from '@/shared/i18n';

/**
 * The rail's two bottom blocks, ported from `SidebarBody.jsx`'s own bottom
 * half — which is TWO blocks, not one:
 *
 *  1. `styles.bottomSection` (`flex: 1; justify-content: flex-end`) INSIDE
 *     the scrollable column: `Buttons.SettingsButton`, a FULL-BLEED divider
 *     (`catalogDivider`, `marginInline: -1rem`), then
 *     `Buttons.AgentHubButton`. That `flex: 1` is what pins the pair to the
 *     bottom of the nav column instead of letting it sit directly under the
 *     last nav group.
 *  2. `styles.footerBlock` — a 3.25rem bar OUTSIDE the scroll area with a
 *     full-width top rule, split by a vertical divider into the "Support
 *     Bot" hit area and the Help Center "?" icon button
 *     (`Buttons.ResourcesButton`, the NON-`fullWidth` variant). When the
 *     deployment has no support assistant, the bar degrades to
 *     `styles.helpCenterFooter`: the same top rule with the full-width
 *     "Help Center" row in it.
 *
 * All three used to be plain stacked rows in one padded column here, which
 * is why the rail showed "Settings / Catalog / Help Center" bunched directly
 * under Artifacts with no bottom bar at all.
 */

/** `SidebarBody.jsx`'s `styles.footerBlock` height. */
const FOOTER_BAR_HEIGHT = '3.25rem';

/**
 * `AgentHubButton.jsx` paints the catalogue pill's icon and label with the
 * `background.button.agentHub.{icon,text}Gradient` tokens, not a flat
 * colour: the icon through a CSS mask, the label through
 * `background-clip: text`. The port had substituted `palette.primary.main`
 * for both, which rendered the row in the primary CYAN while production
 * shows the violet gradient.
 */
const agentHubIconMask = `url("${agentHubIconUrl}")`;

export interface SidebarBottomLinksProps {
  collapsed: boolean;
}

/** Block 1 — Settings + full-bleed divider + Catalog, pinned to the bottom of the nav column. */
export function SidebarBottomLinks({ collapsed }: SidebarBottomLinksProps): ReactNode {
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });

  return (
    <Box sx={bottomSectionSx}>
      <Box sx={sectionSx}>
        <FooterLink
          to="/settings/model-configuration"
          icon={<GearIcon style={{ width: '1rem', height: '1rem' }} />}
          label={t('widgets.sidebar.settings', 'Settings')}
          collapsed={collapsed}
          active={pathname.startsWith('/settings')}
        />
        <Box sx={catalogDividerSx} />
        <CatalogLink
          collapsed={collapsed}
          active={pathname === '/elitea-catalog'}
        />
      </Box>
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

  if (onToggleAssistant === undefined) {
    // `SidebarBody.jsx:304-310` — no assistant, and the rail is expanded:
    // the bar is just the full-width Help Center row under the same rule.
    if (collapsed) return null;
    return (
      <Box sx={helpCenterFooterSx}>
        <FooterLink
          to="/help-center"
          icon={<ResourcesIcon style={{ width: '1.25rem', height: '1.25rem' }} />}
          label={t('widgets.sidebar.helpCenter', 'Help Center')}
          collapsed={false}
          active={onHelp}
        />
      </Box>
    );
  }

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

interface CatalogLinkProps {
  collapsed: boolean;
  active: boolean;
}

/**
 * Ported from `ui/button/AgentHubButton.jsx` — an always-visible, ungated
 * pill (no `PERMISSION_GROUPS` entry in the old app either) navigating to
 * `/elitea-catalog`. Cosmetically distinct from the plain `FooterLink` rows
 * around it, matching the old app's own `background.button.agentHub.*`
 * token family (default/active/hover + inset box-shadow variants) and its
 * icon/text GRADIENTS.
 */
function CatalogLink({ collapsed, active }: CatalogLinkProps): ReactNode {
  const label = t('widgets.sidebar.eliteaCatalog', 'Catalog');
  return (
    <Tooltip
      title={collapsed ? label : ''}
      placement="right"
      enterDelay={500}
      enterNextDelay={500}
    >
      <Box
        component={Link}
        to="/elitea-catalog"
        aria-current={active ? 'page' : undefined}
        data-testid="sidebar-agent-hub-button"
        sx={(theme: Theme) => ({
          width: collapsed ? '2rem' : '100%',
          height: '2rem',
          padding: collapsed ? '0.5rem 0' : '0.5rem',
          borderRadius: theme.vars.shape.radiusMd,
          background: active
            ? theme.vars.palette.background.button.agentHub.active
            : theme.vars.palette.background.button.agentHub.default,
          boxShadow: active
            ? theme.vars.palette.background.button.agentHub.shadowActive
            : theme.vars.palette.background.button.agentHub.shadowDefault,
          display: 'flex',
          justifyContent: collapsed ? 'center' : 'flex-start',
          alignItems: 'center',
          gap: '0.5rem',
          boxSizing: 'border-box',
          textDecoration: 'none',
          '&:hover': {
            background: theme.vars.palette.background.button.agentHub.hover,
            boxShadow: theme.vars.palette.background.button.agentHub.shadowHover,
          },
        })}
      >
        <Box sx={catalogIconSx} />
        {!collapsed && (
          <Typography
            variant="labelSmall"
            sx={catalogLabelSx}
          >
            {label}
          </Typography>
        )}
      </Box>
    </Tooltip>
  );
}

interface FooterLinkProps {
  to: string;
  icon: ReactNode;
  label: string;
  collapsed: boolean;
  active: boolean;
}

function FooterLink({ to, icon, label, collapsed, active }: FooterLinkProps): ReactNode {
  return (
    <Tooltip
      title={collapsed ? label : ''}
      placement="right"
      enterDelay={500}
      enterNextDelay={500}
    >
      <Box
        component={Link}
        to={to}
        // `active` drives the styling below, but TanStack's <Link> only sets
        // aria-current on an EXACT path match. "Settings" points at
        // /settings/model-configuration and is active for every /settings/*
        // route, so assistive technology was told nothing was current while
        // the sidebar showed it selected. The main nav items above already
        // carry aria-current="page"; these three did not.
        aria-current={active ? 'page' : undefined}
        sx={(theme: Theme) => ({
          width: collapsed ? '2rem' : '100%',
          height: '2rem',
          padding: collapsed ? '0.5rem 0' : '0.5rem',
          borderRadius: theme.vars.shape.radiusMd,
          background: active ? theme.vars.palette.background.button.drawerMenu.selected : 'transparent',
          '&:hover': { backgroundColor: theme.vars.palette.background.button.drawerMenu.hover },
          display: 'flex',
          justifyContent: collapsed ? 'center' : 'flex-start',
          alignItems: 'center',
          gap: '0.5rem',
          boxSizing: 'border-box',
          color: active ? theme.vars.palette.text.secondary : theme.vars.palette.text.metrics,
          textDecoration: 'none',
        })}
      >
        {icon}
        {!collapsed && <Typography variant="labelSmall">{label}</Typography>}
      </Box>
    </Tooltip>
  );
}

/** `SidebarBody.jsx`'s `styles.bottomSection`. */
const bottomSectionSx = { flex: 1, display: 'flex', flexDirection: 'column', justifyContent: 'flex-end' } as const;

/** `SidebarBody.jsx`'s `styles.section` — the same padding/gap the nav groups use. */
const sectionSx = {
  paddingBlock: '0.5rem',
  paddingInline: '1rem',
  gap: '0.5rem',
  display: 'flex',
  flexDirection: 'column',
  boxSizing: 'border-box',
} as const;

/** `styles.catalogDivider` — negative inline margin bleeds it to the rail's full width. */
const catalogDividerSx = (theme: Theme) => ({
  marginInline: '-1rem',
  height: '0.0625rem',
  backgroundColor: theme.vars.palette.border.sidebarDivider,
});

const catalogIconSx = (theme: Theme) => ({
  width: '1rem',
  height: '1rem',
  flexShrink: 0,
  background: theme.vars.palette.background.button.agentHub.iconGradient,
  WebkitMaskImage: agentHubIconMask,
  maskImage: agentHubIconMask,
  WebkitMaskRepeat: 'no-repeat',
  maskRepeat: 'no-repeat',
  WebkitMaskSize: 'contain',
  maskSize: 'contain',
  WebkitMaskPosition: 'center',
  maskPosition: 'center',
});

const catalogLabelSx = (theme: Theme) => ({
  flex: 1,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  textAlign: 'left',
  background: theme.vars.palette.background.button.agentHub.textGradient,
  WebkitBackgroundClip: 'text',
  backgroundClip: 'text',
  WebkitTextFillColor: 'transparent',
  color: 'transparent',
});

/** The 1px top rule both footer variants carry (`styles.footerBlock`/`styles.helpCenterFooter`'s `:before`). */
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

const helpCenterFooterSx = (theme: Theme) => ({ ...footerRuleSx(theme), padding: '0.5rem 1rem' });

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
