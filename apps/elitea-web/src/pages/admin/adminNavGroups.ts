/**
 * The admin SPA's navigation DATA — the two groups, their order, their icons
 * and the permission each item is gated on for PRESENTATION only.
 *
 * Split out of `./adminNavItems.ts`, which keeps the nav MODEL (the types and
 * the three functions that read this table). The split is not cosmetic: the
 * item list is an enumeration with one evidence comment per entry, it grows
 * with every ported admin page, and it had reached the 400-line file cap — so
 * the next item could not be added without either deleting somebody's evidence
 * or turning the cap off for the logic beside it.
 *
 * Read `./adminNavItems.ts`'s header first. It carries the four differences
 * from the reference sidebar and the rule that `permissions` here is
 * presentation and never authorisation. Both still apply to every entry below.
 */
import AccountBalanceWalletOutlinedIcon from '@mui/icons-material/AccountBalanceWalletOutlined';
import AssignmentOutlinedIcon from '@mui/icons-material/AssignmentOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import PaletteOutlinedIcon from '@mui/icons-material/PaletteOutlined';
import HistoryOutlinedIcon from '@mui/icons-material/HistoryOutlined';
import HubOutlinedIcon from '@mui/icons-material/HubOutlined';
import MailOutlineIcon from '@mui/icons-material/MailOutlined';
// The reference imports `@mui/icons-material/PeopleOutline`, which MUI 9 no
// longer ships under that name; `PeopleOutlineOutlined` is the same glyph.
import PeopleOutlineIcon from '@mui/icons-material/PeopleOutlineOutlined';
import PlayCircleOutlineOutlinedIcon from '@mui/icons-material/PlayCircleOutlineOutlined';
import PolicyOutlinedIcon from '@mui/icons-material/PolicyOutlined';
import ScheduleOutlinedIcon from '@mui/icons-material/ScheduleOutlined';
import SecurityOutlinedIcon from '@mui/icons-material/SecurityOutlined';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import TuneOutlinedIcon from '@mui/icons-material/TuneOutlined';
import VpnKeyOutlinedIcon from '@mui/icons-material/VpnKeyOutlined';

import type { ComponentType } from 'react';

import type { SvgIconProps } from '@mui/material/SvgIcon';

import { t } from '@/shared/i18n';

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
export function navGroups(): readonly AdminNavGroup[] {
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
          id: 'toolkit-types',
          path: '/toolkits',
          label: t('pages.admin.nav.toolkitTypes', 'Toolkits'),
          icon: BuildOutlinedIcon,
          // The permission every toolkit-type route is gated on server-side
          // (`internal/api/router.go`, `central(admin.ToolkitTypeManagePermission)`),
          // and one this platform ISSUES: shared migration 0114 grants
          // `toolkit_catalogue.type.manage` to the administration-mode
          // super_admin, admin and system. No pylon SECTION name is listed
          // beside it because pylon has no counterpart surface — its only
          // toolkit control is the guardrails deny-list on the Configuration
          // page. See this module's header on why a gate whose permission no
          // seed grants is a nav item that disappears for good.
          anyPermission: ['toolkit_catalogue.type.manage'],
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
          id: 'budgets',
          path: '/budgets',
          label: t('pages.admin.nav.budgets', 'Budgets'),
          icon: AccountBalanceWalletOutlinedIcon,
          // The exact permission `internal/api/router.go` gates every
          // administration-mode budget read on (`requireBudgetsView`), and one
          // that IS issued: shared migration 0062 grants it to both the `admin`
          // and `super_admin` administration roles. See this module's header on
          // why a name nothing grants is a nav item that disappears for good.
          anyPermission: ['models.admin.project_budgets.view'],
        },
        {
          id: 'audit',
          path: '/audit',
          label: t('pages.admin.nav.audit', 'Audit Trail'),
          icon: HistoryOutlinedIcon,
          anyPermission: ['models.admin.audit_trail.view'],
        },
        {
          id: 'tasks',
          path: '/tasks',
          label: t('pages.admin.nav.tasks', 'Tasks'),
          icon: PlayCircleOutlineOutlinedIcon,
          // `runtime.plugins` is what both background-job routes are gated on
          // server-side, and it is the string the LEGACY Tasks page declares
          // (legacy/plugins/admin/module.py:392-421). Shared migration 0060
          // grants it, so this item cannot disappear for want of a grant.
          anyPermission: ['configuration', 'runtime.plugins'],
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
