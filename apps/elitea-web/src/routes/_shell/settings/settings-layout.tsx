import { useEffect } from 'react';

import { Outlet, useLocation, useNavigate, useRouteContext } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useIsPersonalProject } from '@/routes/-guards/personalProject';
import { isPublicProject } from '@/routes/-guards/publicProject';
import { t } from '@/shared/i18n';
import { useIsAnalyticsVisible } from '@/shared/lib/hooks/useIsAnalyticsVisible';
import { useIsUsageVisible } from '@/shared/lib/hooks/useIsUsageVisible';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { SETTINGS_LAYOUT } from '@/shared/ui/settings/settings.constants';
import { type SettingsSection, SettingsDrawer } from '@/shared/ui/settings/SettingsDrawer';
import {
  type SettingsSectionGates,
  buildSettingsSections,
  isTabHidden,
} from './settingsSections';
import { DEFAULT_SETTINGS_TAB, SettingsRedirect } from '@/shared/ui/settings/SettingsRedirect';

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
/**
 * `personal_project_id` off the router context's `auth.getUser()`
 * (`app/router-context.ts`'s `AuthUser`), read STRUCTURALLY rather than
 * imported — `routes/` may not reach into `app/`. The same seam
 * `shared/ui/EntityRail/useRailContext.ts` and
 * `routes/-pages/Notifications.tsx` each already carry for the same field.
 */
interface PersonalProjectIdContext {
  readonly auth?: {
    readonly getUser?: () => { readonly personal_project_id?: string } | undefined;
  };
}

function usePersonalProjectId(): string | undefined {
  const context: unknown = useRouteContext({ strict: false });
  if (typeof context !== 'object' || context === null) return undefined;
  return (context as PersonalProjectIdContext).auth?.getUser?.()?.personal_project_id;
}

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
  // TWO PROJECT-SHAPE GATES THE REFERENCE APPLIES AND THIS PORT DID NOT.
  //
  // `pages/settings/index.jsx:150-166` filters the PROJECT list on more than
  // the two platform flags above:
  //
  //  - `publicOnly` rows (Service Prompts, Environment) appear ONLY in the
  //    tenant's public project. They edit platform-wide service prompts and
  //    the platform environment; in any other project they were dead rows.
  //    Measured against a live deployment in a personal project, production
  //    shows neither.
  //  - `users` disappears in the caller's PERSONAL project
  //    (`isPrivateProject = projectId == user.personal_project_id`). A
  //    one-person project has no membership to manage.
  //  - `project-context` is the mirror image: it is hidden in the PUBLIC
  //    project only.
  //
  // Without these three, a personal project drew three rows production does
  // not have, which is also why "General" (the reference's first row and its
  // default tab) had no room in this drawer.
  //
  // `useIsPersonalProject` is the id comparison the reference makes PLUS the
  // reserved-name check this backend needs — see `routes/-guards/
  // personalProject.ts`. Written here as `selectedProjectId ===
  // personalProjectId` alone, it answered yes for every member of a single
  // shared project (whose `personal_project_id` resolves to that shared
  // project, because provisioning never runs for them), hid Users and
  // redirected `/settings/users` back to General.
  const selectedProjectId = useSelectedProjectStore((state) => state.project?.id ?? '');
  const personalProjectId = usePersonalProjectId();
  const isPersonalProject = useIsPersonalProject(selectedProjectId, personalProjectId);
  const gates: SettingsSectionGates = {
    isPublicProject: isPublicProject(selectedProjectId),
    isPersonalProject,
    analyticsVisible,
    usageVisible,
  };
  const sections: SettingsSection[] = buildSettingsSections(gates);

  /*
   * A HIDDEN TAB MUST ALSO BE UNREACHABLE BY URL.
   *
   * Dropping a row from the drawer stops it being CLICKABLE; it does not stop
   * `/settings/prompts` being typed, bookmarked or linked. Both of those pages
   * gate every one of their own queries on `useIsPublicProject`, so in any
   * other project they render a body with nothing in it — no rows, no error,
   * no request. Measured on a fresh install: the server publishes
   * `public_project_id: 1`, the image ships `VITE_PUBLIC_PROJECT_ID=99`, the
   * user sits in personal project 2, and both screens draw an empty page that
   * looks like a broken deployment rather than a screen that does not apply.
   *
   * The reference guards exactly this, and with a redirect rather than an
   * error (`pages/settings/index.jsx:196-201`: "Guard: hide Service Prompts
   * and Environment for non-Public projects" -> `handleSettingsItemClick(
   * DEFAULT_TAB)`). `users` is guarded the same way for the same reason: a
   * personal project has one member and no membership to manage.
   */
  const location = useLocation();
  const isHiddenTab = isTabHidden(location.pathname.split('/').at(-1), gates);

  useEffect(() => {
    if (!isHiddenTab) return;
    void navigate({
      to: '/settings/$tab',
      params: { tab: DEFAULT_SETTINGS_TAB },
      replace: true,
      search: (previous) => previous,
    });
  }, [isHiddenTab, navigate]);

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
