import { Outlet, useNavigate } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { performLogout } from '@/shared/api/auth';
import { t } from '@/shared/i18n';
import { useIsAnalyticsVisible } from '@/shared/lib/hooks/useIsAnalyticsVisible';
import { useIsUsageVisible } from '@/shared/lib/hooks/useIsUsageVisible';
import { SETTINGS_LAYOUT } from '@/shared/ui/settings/settings.constants';
import { type SettingsSection, SettingsDrawer } from '@/shared/ui/settings/SettingsDrawer';
import { SettingsRedirect } from '@/shared/ui/settings/SettingsRedirect';

/**
 * Top-level Settings page layout. Replaces the placeholder `SettingsLayout`
 * in `route.tsx`. Renders:
 *
 * 1. A left sidebar (`SettingsDrawer`) with project and personal section tabs.
 * 2. An `<Outlet />` for tab-specific content.
 * 3. Inline `SettingsRedirect` for legacy/invalid tab handling.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/settings/index.jsx`.
 */
/** The PERSONAL section's action item that is not a route (see `handleItemClick`). */
const LOGOUT_TAB_ID = 'logout';

export function SettingsLayout() {
  const navigate = useNavigate();
  // The admin Features page's Analytics switch (`analytics_enabled`) — see
  // `useIsAnalyticsVisible`'s doc comment for where this field came from and
  // why hiding the tab here is only half of the gate; the other half is the
  // 403 `internal/api/router.go`'s `requireAnalyticsEnabled` puts on every
  // `/analytics*` route.
  const analyticsVisible = useIsAnalyticsVisible();
  // `cost_budgets_enabled` — the reference gated the same tab on the same key.
  // It defaults CLOSED rather than open, unlike every other flag hook here; see
  // `useIsUsageVisible` for why a tab of structural zeroes is worse than a
  // missing tab.
  const usageVisible = useIsUsageVisible();
  const sections: SettingsSection[] = [
    {
      section: 'PROJECT',
      tabs: [
        {
          id: 'model-configuration',
          label: 'AI Configuration',
        },
        {
          id: 'prompts',
          label: 'Service Prompts',
        },
        {
          id: 'environment',
          label: 'Environment',
        },
        {
          id: 'project-params',
          label: 'Project Params',
        },
        {
          id: 'secrets',
          label: 'Secrets',
        },
        {
          id: 'users',
          label: 'Users',
        },
        ...(usageVisible
          ? [
              {
                id: 'usage',
                label: 'Usage',
              },
            ]
          : []),
        ...(analyticsVisible
          ? [
              {
                id: 'analytics',
                label: 'Analytics',
              },
            ]
          : []),
      ],
    },
    {
      section: 'PERSONAL',
      tabs: [
        {
          id: 'personalization',
          label: 'Personalization',
        },
        // Baseline order (`[fsd]/pages/settings/index.jsx:112-141`):
        // Profile, Preferences, AI Personality, Memory, Personal Tokens,
        // Notifications. Preferences/AI Personality/Memory had no counterpart
        // here at all — the pages existed in production and simply were not
        // ported. `Profile` is still absent; its identity rows (full name,
        // email, user id, last login) and the Log out button that lived with
        // them have no home yet, which is why Log out is a nav item below
        // rather than a control on a page.
        {
          id: 'preferences',
          label: 'Preferences',
        },
        {
          id: 'ai-personality',
          label: 'AI Personality',
        },
        {
          id: 'memory',
          label: 'Memory',
        },
        {
          id: 'tokens',
          label: 'Personal Tokens',
        },
        {
          id: 'notifications',
          label: 'Notifications',
        },
        {
          id: LOGOUT_TAB_ID,
          label: 'Log out',
        },
      ],
    },
  ];

  const handleItemClick = (tabId: string) => {
    // "Log out" is not a tab: there is no `routes/_shell/settings/logout.tsx`,
    // so treating it like one only pushed a URL with no route behind it and
    // `SettingsRedirect` bounced the user straight back into the app with
    // every `el.*` key intact (issue #136 A). `performLogout()` is the real
    // implementation — the `el.` namespace sweep in both storage areas plus
    // the `/forward-auth/logout` handoff that clears the server session
    // cookie — and this is its call site.
    if (tabId === LOGOUT_TAB_ID) {
      performLogout();
      return;
    }
    // NAVIGATE THROUGH THE ROUTER, NOT window.history.
    //
    // This used to be `window.history.replaceState(null, '', `/settings/${tabId}`)`,
    // which broke three things at once:
    //
    //  1. It wrote a path with no base. The SPA is served under `/app/`, so
    //     clicking "Secrets" put `/settings/secrets` in the address bar.
    //     Reloading, bookmarking or sharing that URL reached nginx, which
    //     serves the shell only under /app/, and answered `404 page not
    //     found`. Verified against a live deployment: GET /settings/secrets
    //     is 404, GET /app/settings/secrets is 200.
    //  2. replaceState REPLACES the history entry, so Back could never return
    //     to the tab the user came from.
    //  3. It dropped the search parameters the route validates.
    //
    // `navigate` resolves the route's own path, keeps the base, pushes a real
    // entry, and carries the current search through.
    // Later: add permissions and public project checks here, same as the old
    // app's `useMemo` filter. The analytics check is done above, in the
    // `sections` list itself: the tab is omitted entirely rather than left
    // clickable and redirected here, so `SettingsRedirect` never has to decide
    // what an operator following a stale link to a hidden tab should see.
    void navigate({ to: '/settings/$tab', params: { tab: tabId }, search: (previous) => previous });
  };

  return (
    <Box sx={styles.container}>
      <Box sx={styles.drawer}>
        <SettingsDrawer
          sections={sections}
          onItemClick={handleItemClick}
        />
      </Box>
      {/* A <section>, not a second <main>. `widgets/app-shell/ui/AppShell.tsx`
        * already wraps this whole route in the page's one <main> landmark.
        * So `component="main"` here nested a landmark inside itself, which is
        * invalid. It also left assistive technology with two "main" regions to
        * choose between. Verified on a live deployment:
        * document.querySelector('main main') was non-null on every shell
        * page. */}
      <Box
        component="section"
        aria-label={t('shared.ui.settings.drawer.title', 'Settings')}
        sx={styles.mainContent}
      >
        <Outlet />
      </Box>
      <SettingsRedirect />
    </Box>
  );
}

/** @type {MuiSx} */
const styles: Record<string, SxProps<Theme>> = {
  container: {
    display: 'flex',
    height: '100%',
  },
  drawer: (theme) => ({
    width: SETTINGS_LAYOUT.DRAWER_WIDTH,
    flexShrink: 0,
    height: '100%',
    backgroundColor: theme.vars.palette.background.secondary,
    borderRight: `0.0625rem solid ${theme.vars.palette.border.table ?? 'transparent'}`,
    boxSizing: 'border-box',
  }),
  mainContent: (theme) => ({
    flexGrow: 1,
    height: '100%',
    background: theme.vars.palette.background.settingsPage,
    maxWidth: `calc(100% - ${SETTINGS_LAYOUT.DRAWER_WIDTH})`,
    overflow: 'auto',
  }),
};
