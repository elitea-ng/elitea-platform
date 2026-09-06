/**
 * Pure nav-section data + permission filtering (spec SHELL-001..010). Old
 * app: `[fsd]/widgets/sidebar-root/ui/SidebarBody.jsx`'s `sections` useMemo
 * (lines 93-199) + `lib/constants/sidebar.constants.js`'s
 * `RouteToSideBarItemMap`.
 *
 * `skills`' visibility gate ("hide skills on the public project", old app's
 * `isSelectedProjectPublic`) is wired below via `computeIsSelectedProjectPublic`
 * — `entities/project`'s `isPublicProject` selector plus `shared/config`'s
 * `getConfig()` are both legitimately-downward imports from a widget (same
 * reasoning `pages/agents/lib/isPublicAgentsProject.ts` already documents
 * for the identical two-line body), and neither needs a React hook (`getConfig`
 * is a plain memoized sync read), so this stays a pure function callable
 * straight from `SidebarBody`'s existing `useMemo`.
 *
 * `mcps`' visibility gate (`useIsMcpVisible`, reading the real
 * `PlatformSettings.mcp_enabled` flag) is NOT wired here: unlike the skills
 * gate, the hook that provides it (`features/toolkits/api/useIsMcpVisible.ts`
 * / `features/agents/api/useIsMcpVisible.ts`) is a live `useGetPlatformSettings`
 * TanStack Query call with no accompanying mock in this repo's test fixtures
 * (`src/test/msw/handlers/**` has no `/elitea_core/platform_settings/
 * prompt_lib` handler) — wiring it here would need either a new global
 * handler there or per-test mocks added to every existing test that renders
 * this widget's tree, including `widgets/app-shell/__tests__/
 * AppShell.test.tsx`, which is outside this cluster's file scope. Disclosed
 * as a follow-up rather than silently left as "cannot reach" (which is no
 * longer accurate — it CAN be reached, wiring it safely just needs a change
 * outside this file's ownership fence).
 */
import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';
import { PERMISSION_GROUPS } from '@/shared/lib/permissions';

export type NavItemValue =
  | 'chat'
  | 'agents'
  | 'pipelines'
  | 'skills'
  | 'toolkits'
  | 'mcps'
  | 'credentials'
  | 'applications'
  | 'artifacts';

export interface NavItem {
  readonly value: NavItemValue;
  readonly label: string;
  readonly url: string;
}

export interface NavSection {
  readonly items: readonly NavItem[];
}

/** SHELL-001..009, in the old app's 3-group layout. Route paths verified against `src/routes/_shell/**`. */
export function navSections(): readonly NavSection[] {
  return [
    {
      items: [
        { value: 'chat', label: 'Chats', url: '/chat' },
        { value: 'agents', label: 'Agents', url: '/agents' },
        { value: 'pipelines', label: 'Pipelines', url: '/pipelines' },
      ],
    },
    {
      items: [
        { value: 'skills', label: 'Skills', url: '/skills' },
        // The row reads "Toolkits & Indexes" in production, and it is one row
        // for both things: `SidebarBody.jsx:137` in the old app gives it that
        // label, that breadcrumb and that tooltip. This app shortened it to
        // "Toolkits", which names half of what the row opens. The PAGE title
        // is a separate surface and keeps its own string.
        { value: 'toolkits', label: 'Toolkits & Indexes', url: '/toolkits' },
        { value: 'mcps', label: 'MCPs', url: '/mcps' },
        { value: 'credentials', label: 'Credentials', url: '/credentials' },
        { value: 'applications', label: 'Applications', url: '/apps' },
      ],
    },
    {
      items: [{ value: 'artifacts', label: 'Artifacts', url: '/artifacts' }],
    },
  ];
}

/** `PERMISSION_GROUPS` has no `applications`/`mcps` entry (old app: `mcps` reuses the `toolkits` group's permission list; `applications`/`skills` are ungated). */
function requiredPermissionsFor(value: NavItemValue): readonly string[] | undefined {
  if (value === 'mcps') return PERMISSION_GROUPS.toolkits;
  if (value in PERMISSION_GROUPS) return PERMISSION_GROUPS[value as keyof typeof PERMISSION_GROUPS];
  return undefined;
}

export interface VisibleNavSectionsOptions {
  /** Old app: `isSelectedProjectPublic` — hides `skills` on the public project, in addition to the `PERMISSION_GROUPS` check. Defaults to `false` (skills shown) for callers that don't pass it. */
  readonly isSelectedProjectPublic?: boolean;
}

/** SHELL-010: filters sections/items by the caller's permission set (plus the `skills`/public-project gate); empty sections are dropped entirely (old app: `.filter(section => section.length > 0)`). */
export function visibleNavSections(
  sections: readonly NavSection[],
  permissions: ReadonlySet<string>,
  options: VisibleNavSectionsOptions = {},
): readonly NavSection[] {
  const { isSelectedProjectPublic = false } = options;
  return sections
    .map((section) => ({
      items: section.items.filter((item) => {
        if (item.value === 'skills' && isSelectedProjectPublic) return false;
        const required = requiredPermissionsFor(item.value);
        return !required || required.length === 0 || required.some((permission) => permissions.has(permission));
      }),
    }))
    .filter((section) => section.items.length > 0);
}

/**
 * Old app: `projectId == PUBLIC_PROJECT_ID` (`SidebarBody.jsx`'s
 * `isSelectedProjectPublic`, via `common/constants.js`'s
 * `PUBLIC_PROJECT_ID = +VITE_PUBLIC_PROJECT_ID`). `undefined` (no project
 * selected yet, or the runtime config isn't resolved) is never "public".
 */
export function computeIsSelectedProjectPublic(selectedProjectId: string | undefined): boolean {
  if (selectedProjectId === undefined) return false;
  const config = getConfig();
  if (config.status !== 'ok') return false;
  return isPublicProject(selectedProjectId, config.config.vite_public_project_id);
}

/**
 * SidebarBody's `selectedItem` useMemo — which nav item (if any) matches the
 * current pathname. `/agents-hub` and `/elitea-catalog` deliberately match
 * nothing (old app: explicit early return for both, `SidebarBody.jsx:85`);
 * the catalogue has its own pill in the footer, not a nav row.
 */
export function selectedNavItem(pathname: string, items: readonly NavItem[]): NavItemValue | undefined {
  if (pathname === '/agents-hub' || pathname === '/elitea-catalog') return undefined;
  return items.find((item) => pathname.startsWith(item.url))?.value;
}
