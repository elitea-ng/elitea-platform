/**
 * The admin SPA's navigation model — items, permission filtering, and
 * active-route resolution (issue #225).
 *
 * Unit A14 ported ten admin pages and #223 retired `apps/admin-ui`, which made
 * this SPA the one that ships. Its root route was a bare `<Outlet/>`: every page
 * worked by URL and NOTHING linked them, so an operator opening `/admin/app/`
 * landed on Users with no path to the other nine. This module is the data half
 * of the fix; `AdminNav.tsx` renders it and `AdminLayout.tsx` mounts it.
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/components/Layout/
 * Sidebar.jsx` + `constants/permissions.js`. Its two groups, order, icons and
 * collapse behaviour are reproduced. Four things are NOT:
 *
 *  1. **LiteLLM is absent.** The reference's top group ends with it. That page
 *     was deliberately not ported (`./router.tsx`'s header: #201 replaces
 *     LiteLLM with Bifrost), so there is no `/litellm` route in this bundle. A
 *     nav entry pointing at a route that does not exist is the dead-wiring class
 *     this whole port exists to remove — and `AdminNav.test.tsx` derives the
 *     legal path set from the ROUTER, so re-adding one cannot pass CI.
 *
 *  2. **Service Descriptors is present**, though the reference puts it in no nav
 *     group at all. In `admin_ui` that page is dead code (`routes.js` declares
 *     no route and nothing imports it); we gave it a route, so today it is
 *     reachable only by typing a URL — the exact invisibility #225 is about. It
 *     renders the SERVER's 501 reason ("this platform has no provider hub"),
 *     which is a real answer to a real operator question, and materially
 *     different from a control that silently does nothing: "this platform
 *     cannot do that, and here is why" is information, and an operator who
 *     cannot find out is left to guess. It sits in the bottom group beside the
 *     other platform-wide pages.
 *
 *  3. **`schedules` is labelled "Schedules & Tasks", not "System".** The
 *     reference labels that item "System" while the page it opens is headed
 *     "Schedules & Tasks" (`SchedulesTasks.tsx`). A nav label that does not name
 *     its destination costs the operator a click to find out what it was, and
 *     "System" names nothing in particular on a page that is entirely about
 *     schedules and tasks. The page heading wins; the nav follows it.
 *
 *  4. **`isActiveTab`'s last-path-segment comparison is gone.** The reference
 *     compares the LAST segment of `location.pathname` to the item id, so
 *     `/users/123` — or any nested route — highlights nothing at all. Here the
 *     active item comes from the ROUTER's matched route ids (see
 *     `activeAdminNavItemId`), which include every ancestor of the match, so a
 *     nested route highlights its section by construction.
 *
 * ## `permissions` here is PRESENTATION, never authorisation
 *
 * `./adminUiConfig.ts`'s header has the full account. The Go handler resolves
 * the caller's real administration-mode grants, and it injects an empty list
 * when the resolver refuses. The array is a hint about what to render. It is
 * not a gate. Hiding an item whose page would answer 403 is good UX, and it is
 * all that happens here. The server refuses the request either way, and a typed
 * URL still reaches the page (and still gets refused).
 */
import type { ComponentType } from 'react';

import AssignmentOutlinedIcon from '@mui/icons-material/AssignmentOutlined';
import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import PaletteOutlinedIcon from '@mui/icons-material/PaletteOutlined';
import HistoryOutlinedIcon from '@mui/icons-material/HistoryOutlined';
import HubOutlinedIcon from '@mui/icons-material/HubOutlined';
import MailOutlineIcon from '@mui/icons-material/MailOutlined';
// The reference imports `@mui/icons-material/PeopleOutline`, which MUI 9 no
// longer ships under that name; `PeopleOutlineOutlined` is the same glyph.
import PeopleOutlineIcon from '@mui/icons-material/PeopleOutlineOutlined';
import PolicyOutlinedIcon from '@mui/icons-material/PolicyOutlined';
import ScheduleOutlinedIcon from '@mui/icons-material/ScheduleOutlined';
import SecurityOutlinedIcon from '@mui/icons-material/SecurityOutlined';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import TuneOutlinedIcon from '@mui/icons-material/TuneOutlined';
import VpnKeyOutlinedIcon from '@mui/icons-material/VpnKeyOutlined';
import type { SvgIconProps } from '@mui/material/SvgIcon';

import { t } from '@/shared/i18n';

import { adminUiShowsControlFor } from './adminUiConfig';

export interface AdminNavItem {
  /** Stable id — the test ids and the i18n keys are built from it. */
  readonly id: string;
  /**
   * The route's `path` in `./router.tsx`, which for this flat tree is also its
   * route ID. `AdminNav.test.tsx` asserts every one of these exists in the
   * router's own table, so the nav and the routes cannot drift apart.
   */
  readonly path: string;
  readonly label: string;
  readonly icon: ComponentType<SvgIconProps>;
  /**
   * Any ONE of these makes the item visible. Empty means always visible.
   * Presentation only — see this module's header.
   */
  readonly anyPermission: readonly string[];
}

export interface AdminNavGroup {
  readonly id: 'primary' | 'platform';
  readonly items: readonly AdminNavItem[];
}

/**
 * The route id the ADMIN INDEX resolves to. `router.tsx` renders `AdminUsers`
 * at `/` rather than redirecting to `/users` (its header explains why), so on
 * `/admin/app/` the Users item must light up even though `/users` was never
 * matched. Without this the landing screen is the one screen with no active
 * item, which reads as "the nav is broken" on first contact.
 */
const ADMIN_INDEX_ROUTE_ID = '/';

/** Which item the index route stands in for. Kept next to the alias it serves. */
const INDEX_ALIAS_ITEM_ID = 'users';

/**
 * The reference's `SIDEBAR_PERMISSIONS`, with three corrections.
 *
 * `app-requests` there requires `PERMISSIONS.users.section` (`admin.auth.users`)
 * — a copy-paste from the Users entry that predates the moderation permissions.
 * The ported page reads `admin.moderation.edit` for its decide controls and the
 * platform now issues the whole `admin.moderation.*` family, so the nav asks for
 * the permission that actually governs the page.
 *
 * `service-descriptors` has no reference entry at all (no nav item to gate).
 * `configuration.service_descriptors` is the permission the reference's
 * Configuration SECTION for the same subsystem uses
 * (`CONFIG_SECTION_PERMISSIONS`).
 *
 * ## Every item names a permission THIS platform issues, as well
 *
 * The reference gates four items on pylon SECTION names — `projects`,
 * `projects.projects`, `configuration`, `configuration.roles` — and one on
 * `configuration.service_descriptors`. Pylon registers those names, so they stay
 * here for a pylon-backed deployment. This platform's own administration mode
 * registers none of them: `001_initial.sql` and `migrations/shared/*` seed
 * fully-qualified names only.
 *
 * The nav read that as "the operator lacks the permission" and hid the item.
 * `projects` and `service-descriptors` disappeared from every Go-native admin
 * console, silently, with nothing on screen to explain it. Both now name the
 * permission `internal/api/router.go` resolves for the page they open, beside
 * the pylon name. `roles`, `configuration` and `features` already did.
 *
 * The defect stayed invisible while `adminui/handler.go` HARDCODED a 37-string
 * permission list that echoed the reference's section names back to the browser.
 * That handler resolves the operator's real grants now, so an unissuable name
 * hides an item for good. Add no gate whose permission no seed grants.
 */
function navGroups(): readonly AdminNavGroup[] {
  return [
    {
      id: 'primary',
      items: [
        {
          id: 'users',
          path: '/users',
          label: t('pages.admin.nav.users', 'Users'),
          icon: PeopleOutlineIcon,
          anyPermission: ['admin.auth.users'],
        },
        {
          id: 'roles',
          path: '/roles',
          label: t('pages.admin.nav.roles', 'Roles'),
          icon: SecurityOutlinedIcon,
          anyPermission: ['configuration.roles', 'configuration.roles.permissions.view'],
        },
        {
          id: 'projects',
          path: '/projects',
          label: t('pages.admin.nav.projects', 'Projects'),
          icon: FolderOutlinedIcon,
          // `projects` and `projects.projects` are pylon SECTION names. This
          // platform's administration mode issues neither. It issues
          // `projects.projects.projects.view`, which is also the permission
          // `router.go` resolves for the admin project listing this item opens.
          anyPermission: ['projects', 'projects.projects', 'projects.projects.projects.view'],
        },
        {
          id: 'secrets',
          path: '/secrets',
          label: t('pages.admin.nav.secrets', 'Secrets'),
          icon: VpnKeyOutlinedIcon,
          anyPermission: ['configuration.secrets.secret.list', 'configuration.secrets.secret.create'],
        },
        {
          id: 'app-requests',
          path: '/app-requests',
          label: t('pages.admin.nav.appRequests', 'App Requests'),
          icon: AssignmentOutlinedIcon,
          anyPermission: ['admin.moderation', 'admin.moderation.view'],
        },
      ],
    },
    {
      id: 'platform',
      items: [
        {
          id: 'configuration',
          path: '/configuration',
          label: t('pages.admin.nav.configuration', 'Configuration'),
          icon: SettingsOutlinedIcon,
          anyPermission: ['configuration', 'runtime.plugins'],
        },
        {
          id: 'branding',
          path: '/branding',
          label: t('pages.admin.nav.branding', 'Branding'),
          icon: PaletteOutlinedIcon,
          // `configuration.branding` is what every branding route is gated on
          // server-side (`internal/api/router.go`), granted to the two
          // administration-mode admin roles by migration 0109 (ADR-0024
          // decision 5); `configuration` is the prefix `ExpandPermissions`
          // expands into it, as for `governance` below.
          anyPermission: ['configuration', 'configuration.branding'],
        },
        {
          id: 'email',
          path: '/email',
          label: t('pages.admin.nav.email', 'E-mail'),
          icon: MailOutlineIcon,
          // `runtime.plugins` is what every /admin/email route is gated on
          // server-side (`internal/api/router.go`) — the permission the
          // Configuration page it replaces already required, so no new grant
          // is needed. `configuration` is the prefix `ExpandPermissions`
          // expands into it, as for `branding` above.
          //
          // Both names are ones this platform's administration mode issues.
          // See this module's header on why a gate whose permission no seed
          // grants is a nav item that disappears for good.
          anyPermission: ['configuration', 'runtime.plugins'],
        },
        {
          id: 'features',
          path: '/features',
          label: t('pages.admin.nav.features', 'Features'),
          icon: TuneOutlinedIcon,
          anyPermission: ['configuration', 'runtime.plugins'],
        },
        {
          id: 'service-descriptors',
          path: '/service-descriptors',
          label: t('pages.admin.nav.serviceDescriptors', 'Service Descriptors'),
          icon: HubOutlinedIcon,
          // `configuration.service_descriptors` is a pylon CONFIGURATION-SECTION
          // name, and this platform issues it to nobody.
          // `runtime.airun.serviceproviders` is the permission `router.go`
          // resolves for the listing, and 001_initial.sql grants it to both
          // administration-mode admin roles. Keep both: the section name still
          // reaches a pylon-backed deployment.
          anyPermission: ['configuration.service_descriptors', 'runtime.airun.serviceproviders'],
        },
        {
          id: 'governance',
          path: '/governance',
          label: t('pages.admin.nav.governance', 'LLM Governance'),
          icon: PolicyOutlinedIcon,
          // The permission every governance route is gated on server-side
          // (`internal/api/router.go`, `central("configuration.governance")`),
          // plus the `configuration` prefix that `ExpandPermissions` expands
          // into it. Both are names this platform's administration mode issues
          // — see this module's header on why an unissuable name is a nav item
          // that disappears for good.
          anyPermission: ['configuration', 'configuration.governance'],
        },
        {
          id: 'audit',
          path: '/audit',
          label: t('pages.admin.nav.audit', 'Audit Trail'),
          icon: HistoryOutlinedIcon,
          anyPermission: ['models.admin.audit_trail.view'],
        },
        {
          id: 'schedules',
          path: '/schedules',
          // Not the reference's "System" — see this module's header, point 3.
          label: t('pages.admin.nav.schedules', 'Schedules & Tasks'),
          icon: ScheduleOutlinedIcon,
          anyPermission: ['configuration.scheduling.schedules.view', 'runtime.plugins'],
        },
      ],
    },
  ];
}

/**
 * Every item, ungated. Exists so the drift test can assert that EVERY nav
 * target is a real route regardless of which permissions the current session
 * happens to carry — a permission-filtered list would let a broken path hide
 * behind a missing permission.
 */
export function adminNavGroups(): readonly AdminNavGroup[] {
  return navGroups();
}

/**
 * The groups an operator can see, given a permission probe. Groups that lose
 * every item are dropped entirely, so no empty group renders a stray divider.
 *
 * `shows` is injected rather than imported so tests can drive it directly; it
 * defaults to the real (presentation-only) probe.
 */
export function visibleAdminNavGroups(
  shows: (permission: string) => boolean = adminUiShowsControlFor,
): readonly AdminNavGroup[] {
  return navGroups()
    .map((group) => ({
      ...group,
      items: group.items.filter(
        (item) => item.anyPermission.length === 0 || item.anyPermission.some((permission) => shows(permission)),
      ),
    }))
    .filter((group) => group.items.length > 0);
}

/**
 * Which nav item is active, from the ROUTER's matched route ids.
 *
 * `matchedRouteIds` is `routerState.matches.map((match) => match.routeId)` —
 * TanStack Router's own answer to "what is active", containing the leaf match
 * AND every ancestor. That is what makes nesting work with no string surgery:
 * a future `/users/$userId` match arrives as `['__root__', '/users',
 * '/users/$userId']`, and `/users` is in it, so Users highlights. The reference
 * compared the last PATH SEGMENT (`'123'`) to the item id (`'users'`) and
 * highlighted nothing.
 *
 * Returns the item id, or `undefined` when no nav item owns the current route
 * (a 404, say) — which must stay `undefined` rather than defaulting to the first
 * item, since a nav claiming you are on a page you are not on is worse than one
 * claiming nothing.
 */
export function activeAdminNavItemId(matchedRouteIds: readonly string[]): string | undefined {
  const matched = new Set(matchedRouteIds);
  if (matched.has(ADMIN_INDEX_ROUTE_ID)) return INDEX_ALIAS_ITEM_ID;
  for (const group of navGroups()) {
    for (const item of group.items) {
      if (matched.has(item.path)) return item.id;
    }
  }
  return undefined;
}
