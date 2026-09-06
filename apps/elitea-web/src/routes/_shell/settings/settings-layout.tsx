import { Outlet, useNavigate } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

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
        // ORDER AND LABELS ARE THE PRODUCTION DRAWER'S, not this port's
        // earlier guesses. The baseline
        // (`[fsd]/pages/settings/index.jsx:53-101`) lists AI Providers,
        // Project Context, Secrets, Users, Analytics, Usage in that order and
        // under those names; "AI Configuration" and "Project Params" were
        // labels no shipped build ever showed. The URL slugs stay as they are
        // — they are the route files' own names, and renaming them would
        // break every bookmark.
        {
          id: 'model-configuration',
          label: 'AI Providers',
        },
        {
          id: 'project-params',
          label: 'Project Context',
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
          id: 'secrets',
          label: 'Secrets',
        },
        {
          id: 'users',
          label: 'Users',
        },
        ...(analyticsVisible
          ? [
              {
                id: 'analytics',
                label: 'Analytics',
              },
            ]
          : []),
        ...(usageVisible
          ? [
              {
                id: 'usage',
                label: 'Usage',
              },
            ]
          : []),
      ],
    },
    {
      section: 'PERSONAL',
      tabs: [
        // Baseline order (`[fsd]/pages/settings/index.jsx:104-136`): Profile,
        // Preferences, AI Personality, Memory, Personal Tokens,
        // Notifications.
        //
        // Two rows that used to be here are gone.
        //
        // "Personalization" is the OLD combined screen — user info, persona,
        // context management, voice and sound on one page. Production split
        // it into the four tabs below it, all of which this app already has,
        // so leaving it in the drawer offered every control twice, under a
        // name the current UI does not use. `/settings/personalization` still
        // resolves; it is simply not advertised.
        //
        // "Log out" was an ACTION pretending to be a tab. It now lives where
        // the baseline puts it: a button on the Profile page.
        {
          id: 'profile',
          label: 'Profile',
        },
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
      ],
    },
  ];

  const handleItemClick = (tabId: string) => {
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
