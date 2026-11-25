/**
 * The console's six labelled areas (ADR-0059 decision 6, R3.2, issue 2532).
 *
 * This is the web half of the console item table: for each item, its area,
 * the route it opens, its label and its icon. WHICH items a caller sees comes
 * only from the capability document (`./api/adminConsoleApi`): an item shows
 * when the document opens it, an area shows when it has an item to show, and
 * nothing here names a permission. The server half (areas, items, the
 * permissions that open each) is `internal/adminaccess/console.go`;
 * `AdminNav.test.tsx` holds the two tables to each other through the
 * server's golden documents.
 *
 * The item ids are the server's, so `admin-nav-item-<id>` names the same
 * thing in the console, the document and the access-denied page.
 *
 * Some document items have no page of their own here:
 *
 *  - the gateway tabs (story R3.5a, issue 2535) are still tabs of the
 *    Configuration page, so Configuration `standsFor` them and shows for a
 *    caller the document opens any of them to;
 *  - Identity trust (R3.6, issue 2537) is the trust half of the Identity
 *    providers and SCIM pages, never a page: Identity providers `standsFor`
 *    it, and the two pages read its level to lock the trust controls.
 *
 * Access (R3.6, R3.9) opens the role definitions; the permission matrix of
 * `/roles` is its tab for a holder of the matrix permission, so Access is
 * active on `/roles` too.
 *
 * The two-group navigation of `./adminNavGroups` renders while the
 * `admin_console_areas` switch is off; story R4.9 (issue 2623) removes it.
 */
import AccountBalanceWalletOutlinedIcon from '@mui/icons-material/AccountBalanceWalletOutlined';
import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import AssignmentOutlinedIcon from '@mui/icons-material/AssignmentOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import ContentCutOutlinedIcon from '@mui/icons-material/ContentCutOutlined';
import DevicesOutlinedIcon from '@mui/icons-material/DevicesOutlined';
import FactCheckOutlinedIcon from '@mui/icons-material/FactCheckOutlined';
import FingerprintOutlinedIcon from '@mui/icons-material/FingerprintOutlined';
import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import GavelOutlinedIcon from '@mui/icons-material/GavelOutlined';
import HistoryOutlinedIcon from '@mui/icons-material/HistoryOutlined';
import HubOutlinedIcon from '@mui/icons-material/HubOutlined';
import KeyOutlinedIcon from '@mui/icons-material/KeyOutlined';
import LinkOutlinedIcon from '@mui/icons-material/LinkOutlined';
import MailOutlineIcon from '@mui/icons-material/MailOutlined';
import PaletteOutlinedIcon from '@mui/icons-material/PaletteOutlined';
import PeopleOutlineIcon from '@mui/icons-material/PeopleOutlineOutlined';
import PolicyOutlinedIcon from '@mui/icons-material/PolicyOutlined';
import ScheduleOutlinedIcon from '@mui/icons-material/ScheduleOutlined';
import SecurityOutlinedIcon from '@mui/icons-material/SecurityOutlined';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import StopCircleOutlinedIcon from '@mui/icons-material/StopCircleOutlined';
import SyncAltOutlinedIcon from '@mui/icons-material/SyncAltOutlined';
import TuneOutlinedIcon from '@mui/icons-material/TuneOutlined';
import VpnKeyOutlinedIcon from '@mui/icons-material/VpnKeyOutlined';

import type { ComponentType } from 'react';

import type { SvgIconProps } from '@mui/material/SvgIcon';

import { t } from '@/shared/i18n';

/** The six areas, in console order (the server's `adminaccess.Areas()`). */
type AdminConsoleAreaId =
  | 'people_access'
  | 'projects'
  | 'models_spend'
  | 'agents_governance'
  | 'security'
  | 'platform';

export interface AdminConsoleNavItem {
  /** The server's item id. */
  readonly id: string;
  readonly area: AdminConsoleAreaId;
  /** The route id in `./router.tsx` the item opens. */
  readonly path: string;
  readonly label: string;
  readonly icon: ComponentType<SvgIconProps>;
  /** Further route ids the item is active on and guards. */
  readonly alsoOn?: readonly string[];
  /** Document items whose page is still a tab of this one. */
  readonly standsFor?: readonly string[];
  /** False while the server table leaves the item out of the document. */
  readonly available: boolean;
}

export interface AdminConsoleNavArea {
  readonly id: AdminConsoleAreaId;
  readonly label: string;
  readonly items: readonly AdminConsoleNavItem[];
}

/** The document's answer for one item: `undefined` while unknown, `null` when it does not open. */
export type ConsoleItemLevel = (itemId: string) => 'read' | 'write' | null | undefined;

function areaLabels(): Record<AdminConsoleAreaId, string> {
  return {
    people_access: t('pages.admin.console.area.people_access', 'People and access'),
    projects: t('pages.admin.console.area.projects', 'Projects'),
    models_spend: t('pages.admin.console.area.models_spend', 'Models and spend'),
    agents_governance: t('pages.admin.console.area.agents_governance', 'Agents and governance'),
    security: t('pages.admin.console.area.security', 'Security'),
    platform: t('pages.admin.console.area.platform', 'Platform'),
  };
}

const AREA_ORDER: readonly AdminConsoleAreaId[] = [
  'people_access',
  'projects',
  'models_spend',
  'agents_governance',
  'security',
  'platform',
];

function item(
  id: string,
  area: AdminConsoleAreaId,
  path: string,
  label: string,
  icon: ComponentType<SvgIconProps>,
  extra: Partial<Pick<AdminConsoleNavItem, 'alsoOn' | 'standsFor' | 'available'>> = {},
): AdminConsoleNavItem {
  return { id, area, path, label, icon, available: true, ...extra };
}

/** Every item of the table, in console order, available or not. */
export function adminConsoleNavItems(): readonly AdminConsoleNavItem[] {
  return [
    item('users', 'people_access', '/users', t('pages.admin.nav.users', 'Users'), PeopleOutlineIcon),
    item('access', 'people_access', '/access', t('pages.admin.nav.access', 'Access'), SecurityOutlinedIcon, {
      alsoOn: ['/roles'],
    }),
    item(
      'identity-providers',
      'people_access',
      '/identity-providers',
      t('pages.admin.nav.identityProviders', 'Identity providers'),
      FingerprintOutlinedIcon,
      { standsFor: ['identity-trust'] },
    ),
    item('scim', 'people_access', '/scim', t('pages.admin.nav.scim', 'SCIM'), SyncAltOutlinedIcon),
    item('projects', 'projects', '/projects', t('pages.admin.nav.projects', 'Projects'), FolderOutlinedIcon),
    item('app-requests', 'projects', '/app-requests', t('pages.admin.nav.appRequests', 'App Requests'), AssignmentOutlinedIcon),
    item('llm-governance', 'models_spend', '/governance', t('pages.admin.nav.governance', 'LLM Governance'), PolicyOutlinedIcon),
    item('budgets-alerts', 'models_spend', '/budgets', t('pages.admin.nav.budgets', 'Budgets'), AccountBalanceWalletOutlinedIcon),
    item('agent-registry', 'agents_governance', '/agent-registry', t('pages.admin.nav.agentRegistry', 'Agent registry'), FactCheckOutlinedIcon),
    item(
      'connector-requests',
      'agents_governance',
      '/connector-requests',
      t('pages.admin.nav.connectorRequests', 'Connector requests'),
      LinkOutlinedIcon,
    ),
    item(
      'delegation-review',
      'agents_governance',
      '/delegation-review',
      t('pages.admin.nav.delegationReview', 'Delegations awaiting review'),
      AccountTreeOutlinedIcon,
    ),
    item('toolkits', 'agents_governance', '/toolkits', t('pages.admin.nav.toolkitTypes', 'Toolkits'), BuildOutlinedIcon),
    item(
      'credential-inventory',
      'agents_governance',
      '/credential-inventory',
      t('pages.admin.nav.credentialInventory', 'Credential inventory'),
      KeyOutlinedIcon,
    ),
    item(
      'permission-trimming',
      'agents_governance',
      '/permission-trimming',
      t('pages.admin.nav.permissionTrimming', 'Permission trimming'),
      ContentCutOutlinedIcon,
    ),
    item('kill-switch', 'agents_governance', '/kill-switch', t('pages.admin.nav.killSwitch', 'Kill switch'), StopCircleOutlinedIcon),
    item('audit-trail', 'security', '/audit', t('pages.admin.nav.audit', 'Audit Trail'), HistoryOutlinedIcon),
    item('data-retention', 'security', '/retention', t('pages.admin.nav.retention', 'Data retention'), GavelOutlinedIcon),
    item('configuration', 'platform', '/configuration', t('pages.admin.nav.configuration', 'Configuration'), SettingsOutlinedIcon, {
      standsFor: [
        'providers-models',
        'models-pricing',
        'tool-prices',
        'usage',
        'logs',
      ],
    }),
    item('features', 'platform', '/features', t('pages.admin.nav.features', 'Features'), TuneOutlinedIcon),
    item('branding', 'platform', '/branding', t('pages.admin.nav.branding', 'Branding'), PaletteOutlinedIcon),
    item('email', 'platform', '/email', t('pages.admin.nav.email', 'E-mail'), MailOutlineIcon),
    item(
      'schedules-tasks',
      'platform',
      '/schedules',
      t('pages.admin.console.item.schedules-tasks', 'Schedules and tasks'),
      ScheduleOutlinedIcon,
      { alsoOn: ['/tasks'] },
    ),
    item(
      'service-descriptors',
      'platform',
      '/service-descriptors',
      t('pages.admin.nav.serviceDescriptors', 'Service Descriptors'),
      HubOutlinedIcon,
    ),
    item('secrets', 'platform', '/secrets', t('pages.admin.nav.secrets', 'Secrets'), VpnKeyOutlinedIcon),
    item(
      'client-applications',
      'platform',
      '/client-applications',
      t('pages.admin.nav.clientApplications', 'Client applications'),
      DevicesOutlinedIcon,
    ),
  ];
}

/** Every area with every item of the table, ungated: the drift tests read it. */
export function adminConsoleNavAreas(): readonly AdminConsoleNavArea[] {
  const labels = areaLabels();
  const items = adminConsoleNavItems();
  return AREA_ORDER.map((id) => ({ id, label: labels[id], items: items.filter((entry) => entry.area === id) }));
}

/**
 * Whether the document opens the item: `true`, `false`, or `undefined` while
 * the document is unknown. An unavailable item is never closed: the document
 * cannot list it, so its absence says nothing.
 */
export function consoleItemOpen(entry: AdminConsoleNavItem, level: ConsoleItemLevel): boolean | undefined {
  if (!entry.available) return undefined;
  const levels = [entry.id, ...(entry.standsFor ?? [])].map((id) => level(id));
  if (levels.some((answer) => answer === 'read' || answer === 'write')) return true;
  return levels.some((answer) => answer === undefined) ? undefined : false;
}

/**
 * The areas the document opens, each with the items it opens; an area with
 * none is left out.
 */
export function visibleConsoleNavAreas(level: ConsoleItemLevel): readonly AdminConsoleNavArea[] {
  const shown = (entry: AdminConsoleNavItem): boolean => consoleItemOpen(entry, level) === true;
  return adminConsoleNavAreas()
    .map((area) => ({ ...area, items: area.items.filter(shown) }))
    .filter((area) => area.items.length > 0);
}

/**
 * The item that owns the matched route, from the router's matched route ids
 * (leaf and ancestors). The index route `/` renders Users, so it is Users'.
 */
export function consoleNavItemForRoute(matchedRouteIds: readonly string[]): AdminConsoleNavItem | undefined {
  const matched = new Set(matchedRouteIds);
  const items = adminConsoleNavItems();
  if (matched.has('/')) return items.find((entry) => entry.id === 'users');
  return items.find((entry) => matched.has(entry.path) || (entry.alsoOn ?? []).some((path) => matched.has(path)));
}
