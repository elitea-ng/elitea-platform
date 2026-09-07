/**
 * The admin nav (issue #225).
 *
 * The failure this suite exists to prevent is not "the nav looks wrong". It is
 * the two shapes that a rendered nav hides:
 *
 *  - **an item pointing at a route that does not exist.** `/litellm` is the live
 *    example: the reference sidebar has that entry and this bundle deliberately
 *    has no such route (#201 replaces LiteLLM with Bifrost). A hand-written list
 *    of "routes that exist" in this file would go stale the first time somebody
 *    renamed a route, which is exactly how a nav and a route table drift apart —
 *    so the legal set is read out of `createAdminRouter()`'s OWN table.
 *  - **an active-state rule that silently stops matching.** The reference
 *    compares the last path segment to the item id, so any nested or deep-linked
 *    route highlights nothing at all, on a screen that otherwise looks fine.
 *
 * Contrast is asserted here too, from the brand pack, because the E2E axe
 * fixture disables `color-contrast` wholesale — nothing else in this repo would
 * notice an unreadable label in one of the two schemes.
 */
import { RouterProvider } from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { parseColor } from '@/shared/brand/color';

import { installWebStorageShim } from '../../test/webstorage';
import {
  activeAdminNavItemId,
  adminNavGroups,
  visibleAdminNavGroups,
} from './adminNavItems';
import { readPersistedAdminNavCollapsed, useAdminNavCollapsedStore } from './adminNavCollapsed';
import { adminApiBaseUrl } from './adminUiConfig';
import { createAdminRouter } from './router';
import {
  COLLAPSED_SIDE_BAR_WIDTH_REM,
  SIDE_BAR_WIDTH_REM,
} from '@/shared/ui/layout/sidebarWidth';

installWebStorageShim();

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * Every permission any nav item asks for. Derived from the nav, so it is a
 * "this session carries everything" probe for the RENDERING tests below, never
 * a statement about what the server issues. The two catalogues further down
 * carry that.
 */
const ALL_PERMISSIONS = adminNavGroups().flatMap((group) =>
  group.items.flatMap((item) => item.anyPermission),
);

interface AdminUiConfigWindow {
  admin_ui_config?: { permissions?: readonly string[]; user_name?: string };
}

function setPermissions(permissions: readonly string[], userName = 'ops@example.com'): void {
  (window as unknown as AdminUiConfigWindow).admin_ui_config = { permissions, user_name: userName };
}

/**
 * Mounts the REAL admin router at `path`.
 *
 * `createAdminRouter()` builds its own browser history (it takes no options —
 * adding a test-only `history` parameter to production code would be a seam
 * nothing else needs), so the location is staged through jsdom's own history
 * first. `basepath` is `/admin/app`, so the staged URL carries that prefix
 * exactly as the served SPA does.
 */
async function mountAdmin(path = '/users'): Promise<void> {
  window.history.replaceState(null, '', `/admin/app${path}`);
  const router = createAdminRouter();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  await screen.findByTestId('admin-nav');
}

function navItem(id: string): HTMLElement {
  return screen.getByTestId(`admin-nav-item-${id}`);
}

beforeEach(() => {
  setPermissions(ALL_PERMISSIONS);
  useAdminNavCollapsedStore.setState({ collapsed: false });
});

afterEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  delete (window as unknown as AdminUiConfigWindow).admin_ui_config;
});

/* ── the drift gate ────────────────────────────────────────────────────── */

describe('every nav target is a real route', () => {
  it('points only at paths the admin router actually declares', () => {
    // Derived from the ROUTER, never from a list maintained here — a
    // hand-written set is how the nav and the routes drift apart, and it would
    // have gone on passing after a route was renamed or removed.
    const declared = new Set(Object.keys(createAdminRouter().routesById));
    const targets = adminNavGroups().flatMap((group) => group.items.map((item) => item.path));

    expect(targets.length).toBeGreaterThan(0);
    for (const target of targets) {
      expect(declared, `nav item "${target}" has no route`).toContain(target);
    }
  });

  it('has no LiteLLM entry, because there is no LiteLLM route', () => {
    // The reference's top group ends with LiteLLM. That page was deliberately
    // not ported (#201 → Bifrost). Asserted from both ends so re-adding the item
    // fails here even if somebody also added a stub route.
    const declared = Object.keys(createAdminRouter().routesById);
    expect(declared).not.toContain('/litellm');
    const ids = adminNavGroups().flatMap((group) => group.items.map((item) => item.id));
    expect(ids).not.toContain('litellm');
  });

  it('offers every page the router serves except the index alias', () => {
    // The other direction: a page that exists but is in no group is reachable
    // only by typing a URL, which is the whole defect #225 describes. Service
    // Descriptors is the one this caught — the reference has it in no nav group
    // at all.
    const routed = Object.keys(createAdminRouter().routesById).filter(
      (id) => id !== '__root__' && id !== '/',
    );
    const targets = new Set(adminNavGroups().flatMap((group) => group.items.map((item) => item.path)));
    for (const route of routed) {
      expect([...targets], `route "${route}" is reachable by URL but from no nav item`).toContain(route);
    }
  });
});

/* ── permission filtering (presentation only) ──────────────────────────── */

/**
 * The administration-mode permissions THIS platform seeds, transcribed from
 * `services/elitea-main/internal/db`'s `internal/infra/db/migrations/
 * 001_initial.sql` and `services/elitea-main/migrations/shared/*.sql`.
 *
 * Duplicated here on purpose, and it is the one hand-written list in this file
 * — because it is not a restatement of this app's own data (the drift gate
 * above reads the ROUTER for that), it is the SERVER's contract. A nav item
 * that no seeded grant can satisfy is hidden from every operator forever, with
 * no error and nothing on screen to explain it: the same "hidden by an
 * unsatisfiable gate" shape as a control wired to nothing.
 *
 * This list replaces the 37 strings `adminui/handler.go` used to HARDCODE for
 * every session it accepted. That handler resolves the caller's real grants
 * now, so the old list stopped describing anything. It also stopped catching
 * the defect it was written for: it carried the reference's invented section
 * names, so the `projects` and `service-descriptors` gates passed here and hid
 * both items in the browser.
 */
const PLATFORM_ISSUED_PERMISSIONS = new Set([
  // 001_initial.sql, administration mode, super_admin and admin.
  'admin.auth.users',
  'admin.auth.users.super_admin',
  'admin.moderation',
  'configuration.roles.permissions.view',
  'configuration.roles.user_project_permissions.edit',
  'configuration.roles.user_project_permissions.view',
  'models.admin.project_budgets.edit',
  'models.admin.project_budgets.view',
  'modes.users',
  'projects.projects.project.create',
  'projects.projects.project.delete',
  'projects.projects.projects.view',
  'provider_hub.descriptor.register',
  'runtime.admin.published_agents',
  'runtime.airun.serviceproviders',
  'runtime.plugins',
  // migrations/shared/0082_admin_panel_permissions.sql.
  'admin.moderation.edit',
  'configuration.governance',
  // migrations/shared/0109_branding_permission.sql (ADR-0024 decision 5),
  // administration mode, super_admin and admin.
  'configuration.branding',
  'configuration.roles.permissions.edit',
  'configuration.scheduling.schedules.edit',
  'configuration.scheduling.schedules.view',
  'configuration.users.users.create',
  'configuration.users.users.edit',
  'models.admin.audit_trail.view',
  'projects.projects.projects.edit',
  // migrations/shared/0085_project_member_and_role_listings_administration.sql.
  'configuration.roles.roles.view',
  'configuration.users.users.view',
  // migrations/shared/0087_administration_secret_permissions.sql.
  'configuration.secrets.secret.create',
  'configuration.secrets.secret.delete',
  'configuration.secrets.secret.edit',
  'configuration.secrets.secret.view',
  // migrations/shared/0114_toolkit_type_policy.sql, administration mode,
  // super_admin, admin and system. It stands alone in its nav item: the legacy
  // platform has no toolkit-catalogue surface, so there is no pylon section
  // name to list beside it.
  'toolkit_catalogue.type.manage',
]);

/**
 * The names the nav also asks for that NO administration-mode seed in this
 * repository grants.
 *
 * Six are the reference sidebar's pylon SECTION names. One,
 * `admin.moderation.view`, this platform does seed — in the DEFAULT mode
 * (`migrations/shared/0077_moderation_permissions.sql`), which the admin
 * console never resolves.
 *
 * They stay in the nav because a pylon-backed deployment registers them, and
 * the Go handler resolves whatever `auth_core__role_permission` holds there.
 * They are listed here so each one is a DECISION rather than a typo, and so an
 * item that names ONLY these fails the test above.
 */
const NOT_SEEDED_IN_ADMINISTRATION = new Set([
  'admin.moderation.view',
  'configuration',
  'configuration.roles',
  'configuration.secrets.secret.list',
  'configuration.service_descriptors',
  'projects',
  'projects.projects',
]);

describe('permission gates name permissions the platform issues', () => {
  it('gives every item a permission a seeded administration role holds', () => {
    // The assertion the old HANDLER_ISSUED_PERMISSIONS list could not make.
    // An item gated ONLY on pylon section names renders on a pylon deployment
    // and on no Go-native one, and nothing else in the repo would notice.
    for (const group of adminNavGroups()) {
      for (const item of group.items) {
        const issued = item.anyPermission.filter((permission) =>
          PLATFORM_ISSUED_PERMISSIONS.has(permission),
        );
        expect(
          issued,
          `nav item "${item.id}" gates only on ${item.anyPermission.join(', ')}, which no administration-mode seed grants — the item is invisible on this platform`,
        ).not.toHaveLength(0);
      }
    }
  });

  it('spells every permission the way one of the two catalogues spells it', () => {
    for (const group of adminNavGroups()) {
      for (const item of group.items) {
        for (const permission of item.anyPermission) {
          expect(
            PLATFORM_ISSUED_PERMISSIONS.has(permission) || NOT_SEEDED_IN_ADMINISTRATION.has(permission),
            `nav item "${item.id}" gates on "${permission}", which neither this platform nor pylon issues`,
          ).toBe(true);
        }
      }
    }
  });

  it('leaves every item visible to the operator this platform seeds', () => {
    // The end-to-end consequence of the two tests above: a Go-native
    // administrator sees all ten. A NARROWER deployment can still hide an item
    // — that is the feature — but a name no seed grants cannot.
    const groups = visibleAdminNavGroups((permission) => PLATFORM_ISSUED_PERMISSIONS.has(permission));
    const visible = groups.flatMap((group) => group.items.map((item) => item.id));
    const all = adminNavGroups().flatMap((group) => group.items.map((item) => item.id));
    expect(visible).toEqual(all);
  });

  it('leaves every item visible to a pylon-backed deployment too', () => {
    const both = new Set([...PLATFORM_ISSUED_PERMISSIONS, ...NOT_SEEDED_IN_ADMINISTRATION]);
    const groups = visibleAdminNavGroups((permission) => both.has(permission));
    const visible = groups.flatMap((group) => group.items.map((item) => item.id));
    const all = adminNavGroups().flatMap((group) => group.items.map((item) => item.id));
    expect(visible).toEqual(all);
  });
});

describe('permission filtering', () => {
  it('hides an item whose permissions the session does not carry', () => {
    const groups = visibleAdminNavGroups((permission) => permission !== 'models.admin.audit_trail.view');
    const ids = groups.flatMap((group) => group.items.map((item) => item.id));
    expect(ids).not.toContain('audit');
    expect(ids).toContain('users');
  });

  it('shows an item when ANY of its permissions is held, not all of them', () => {
    // `roles` asks for `configuration.roles` OR the permissions view. An
    // all-of rule would hide it from an operator who legitimately has one.
    const groups = visibleAdminNavGroups((permission) => permission === 'configuration.roles.permissions.view');
    expect(groups.flatMap((group) => group.items.map((item) => item.id))).toContain('roles');
  });

  it('separates the two groups with exactly one divider, and adds none for a group that is gone', async () => {
    /*
     * The reference's structure: a divider between the two groups, and one
     * above the user footer. Mutation testing found the whole `{index > 0 &&
     * <AdminNavDivider/>}` guard unprotected — `>` flipped, `&&` flipped, the
     * index changed — because nothing counted them. A guard that fires on the
     * FIRST group instead of the second puts a rule above Users and none
     * between the groups, which is a different nav that no other assertion here
     * distinguishes.
     */
    await mountAdmin();
    const nav = screen.getByTestId('admin-nav');
    expect(nav.querySelectorAll('hr')).toHaveLength(2);

    // With only the first group visible there is nothing to separate, so the
    // between-groups divider must not appear — only the footer's.
    cleanup();
    setPermissions(['admin.auth.users']);
    await mountAdmin();
    expect(screen.getByTestId('admin-nav').querySelectorAll('hr')).toHaveLength(1);
  });

  it('drops a group that loses every item, rather than rendering an empty one', () => {
    const groups = visibleAdminNavGroups((permission) =>
      ['admin.auth.users', 'configuration.roles', 'projects', 'configuration.secrets.secret.list', 'admin.moderation'].includes(
        permission,
      ),
    );
    expect(groups.map((group) => group.id)).toEqual(['primary']);
  });

  it('renders nothing at all when the session carries no admin permission', () => {
    expect(visibleAdminNavGroups(() => false)).toEqual([]);
  });

  it('reads the injected config when no probe is supplied', async () => {
    setPermissions(ALL_PERMISSIONS.filter((permission) => permission !== 'models.admin.audit_trail.view'));
    await mountAdmin();
    expect(screen.queryByTestId('admin-nav-item-audit')).toBeNull();
    expect(navItem('users')).toBeInTheDocument();
  });
});

/* ── active route ──────────────────────────────────────────────────────── */

describe('active route', () => {
  it('marks the item for the route the router matched', async () => {
    await mountAdmin('/roles');
    expect(navItem('roles')).toHaveAttribute('aria-current', 'page');
    expect(navItem('users')).not.toHaveAttribute('aria-current');
  });

  it('marks Users on the index route, which renders the Users page', async () => {
    // `router.tsx` renders `AdminUsers` at `/` rather than redirecting. Without
    // the alias the LANDING screen would be the one screen with nothing active.
    await mountAdmin('/');
    expect(navItem('users')).toHaveAttribute('aria-current', 'page');
  });

  it('marks the parent section for a NESTED route, where the reference marked nothing', () => {
    /*
     * `matches` is what `useRouterState` hands the nav, leaf-last with every
     * ancestor — the shape the real router produces (pinned by the next test).
     * A nested `/users/$userId` therefore contains `/users`.
     *
     * The reference's `isActiveTab` split the pathname and compared the LAST
     * segment (`'123'`) to the item id (`'users'`): nothing matched, and a deep
     * link highlighted no item at all.
     */
    expect(activeAdminNavItemId(['__root__', '/users', '/users/$userId'])).toBe('users');
    expect(activeAdminNavItemId(['__root__', '/schedules', '/schedules/$scheduleId/history'])).toBe(
      'schedules',
    );
  });

  it('is fed the exact match shape the real router produces', async () => {
    // Pins the contract the previous test relies on. If TanStack ever stopped
    // including ancestors in `matches`, nesting would break silently and the
    // synthetic test above would keep passing on its own.
    window.history.replaceState(null, '', '/admin/app/secrets');
    const router = createAdminRouter();
    await router.load();
    expect(router.state.matches.map((match) => match.routeId)).toEqual(['__root__', '/secrets']);
    // 30s, not vitest's 5s default. `router.load()` here is not a stub: it
    // resolves the REAL route tree and pulls in the matched page's module
    // graph, which is the whole point of the test — a synthetic match shape
    // would not notice TanStack changing what it puts in `matches`. That
    // import costs a second or two warm, and several under the coverage
    // instrumentation CI runs it with, so the default budget failed this
    // sharded job while the same test passed on its own (issue: the shard
    // reported `Test timed out in 5000ms` at 5006ms). The assertion is
    // unchanged; only the time it is allowed to take is stated deliberately
    // instead of inherited.
  }, 30_000);

  it('marks nothing when no nav item owns the route', () => {
    // Better than defaulting to the first item: a nav claiming you are on a page
    // you are not on is worse than one claiming nothing.
    expect(activeAdminNavItemId(['__root__'])).toBeUndefined();
    expect(activeAdminNavItemId([])).toBeUndefined();
  });

  it('does not confuse two items whose paths share a prefix', () => {
    // `/secrets` and `/service-descriptors` share `/se`. Route-id matching is
    // immune by construction; a `startsWith` rule would not be.
    expect(activeAdminNavItemId(['__root__', '/service-descriptors'])).toBe('service-descriptors');
    expect(activeAdminNavItemId(['__root__', '/secrets'])).toBe('secrets');
  });

  it('follows a click on a nav item', async () => {
    await mountAdmin('/users');
    await userEvent.click(navItem('projects'));
    expect(await screen.findByTestId('admin-nav-item-projects')).toHaveAttribute('aria-current', 'page');
    expect(window.location.pathname).toBe('/admin/app/projects');
  });
});

/* ── collapsed rendering + persistence ─────────────────────────────────── */

describe('collapsed state', () => {
  it('hides the labels but keeps every item reachable', async () => {
    await mountAdmin();
    expect(screen.getByText('Schedules & Tasks')).toBeInTheDocument();

    await userEvent.click(screen.getByTestId('admin-nav-collapse-toggle'));

    expect(screen.queryByText('Schedules & Tasks')).toBeNull();
    // The LINK survives — collapsing must not remove navigation, only its text.
    expect(navItem('schedules')).toBeInTheDocument();
    expect(screen.queryByText('Elitea Admin')).toBeNull();
  });

  it('actually narrows the rail', async () => {
    // Mutation testing found this: swapping the two widths changed nothing any
    // test could see, so the nav could have "collapsed" to full width — labels
    // gone, rail unchanged — and every other test here would still have passed.
    //
    // Asserted against the SHARED constants, not literals. The literals were
    // `13.75rem`/`3.75rem` — this rail's own numbers, which disagreed with the
    // main app's rail. Restating them here is what let the two drift apart
    // while both files' tests stayed green, so the assertion now fails if the
    // rails stop sharing one definition rather than if a number changes.
    await mountAdmin();
    const nav = screen.getByTestId('admin-nav');
    expect(getComputedStyle(nav).width).toBe(SIDE_BAR_WIDTH_REM);

    await userEvent.click(screen.getByTestId('admin-nav-collapse-toggle'));
    expect(getComputedStyle(nav).width).toBe(COLLAPSED_SIDE_BAR_WIDTH_REM);
  });

  it('points the chevron at what the click will do', async () => {
    // The other half of the same class: `collapsed ? <ChevronRight/> :
    // <ChevronLeft/>` swapped is an arrow pointing the wrong way, which the
    // aria-label assertions do not see. MUI icons carry their own data-testid.
    await mountAdmin();
    const toggle = screen.getByTestId('admin-nav-collapse-toggle');
    expect(within(toggle).getByTestId('ChevronLeftIcon')).toBeInTheDocument();

    await userEvent.click(toggle);
    expect(within(toggle).getByTestId('ChevronRightIcon')).toBeInTheDocument();
  });

  it('keeps every collapsed item named, since only an icon is left', async () => {
    // Collapsed, the label is gone and the tooltip is the ONLY thing that says
    // what the icon is — for a sighted user hovering and for a screen reader
    // alike. Mutation testing found `title={collapsed ? item.label : ''}`
    // unprotected: swapped, the tooltip appears when the label is already
    // visible and vanishes when it is the only cue left.
    await mountAdmin();
    const nav = screen.getByTestId('admin-nav');
    await userEvent.click(screen.getByTestId('admin-nav-collapse-toggle'));

    expect(within(nav).queryByText('Schedules & Tasks')).toBeNull();
    expect(within(nav).getByRole('link', { name: 'Schedules & Tasks' })).toBeInTheDocument();
  });

  it('drops the theme toggle from the collapsed header, which has no room for it', async () => {
    // Mutation survivor: `{!collapsed && <ThemeModeToggle />}` flipped keeps a
    // three-button control in a 3.75rem rail, overlapping the logo. Nothing
    // else here looked at the header's contents.
    //
    // The group is named 'Theme'. It was 'View toggle' — `TabGroupButton`'s
    // generic default — until `ThemeModeToggle` began passing its own
    // `ariaLabel`. A theme selector named "View toggle" told a screen-reader
    // user nothing, so the NAME is the fix and this assertion follows it.
    await mountAdmin();
    expect(screen.getByRole('group', { name: 'Theme' })).toBeInTheDocument();

    await userEvent.click(screen.getByTestId('admin-nav-collapse-toggle'));
    expect(screen.queryByRole('group', { name: 'Theme' })).toBeNull();
  });

  it('persists through localStorage, under its own key', async () => {
    await mountAdmin();
    await userEvent.click(screen.getByTestId('admin-nav-collapse-toggle'));

    expect(readPersistedAdminNavCollapsed()).toBe(true);
    // Namespaced (§5.4), so the logout sweep reaches it…
    expect(window.localStorage.getItem('el.admin.nav.collapsed')).toBe('1');
    // …and NOT aliased onto the product sidebar's key: same origin, different
    // SPA, unrelated preference.
    expect(window.localStorage.getItem('el.sidebar.collapsed')).toBeNull();
  });

  it('hydrates the persisted value on mount', async () => {
    window.localStorage.setItem('el.admin.nav.collapsed', '1');
    await mountAdmin();
    expect(screen.queryByText('Schedules & Tasks')).toBeNull();
  });

  it('is toggled by the logo button too, as in the reference', async () => {
    await mountAdmin();
    const nav = screen.getByRole('navigation', { name: 'Admin navigation' });
    // Scoped to the nav: "Users" is also the PAGE heading behind it, so an
    // unscoped query would pass whatever the nav did.
    expect(within(nav).getByText('Users')).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('admin-nav-logo-toggle'));
    expect(within(nav).queryByText('Users')).toBeNull();
  });
});

/* ── accessibility ─────────────────────────────────────────────────────── */

describe('accessibility', () => {
  it('is a navigation landmark with an accessible name', async () => {
    await mountAdmin();
    expect(screen.getByRole('navigation', { name: 'Admin navigation' })).toBeInTheDocument();
  });

  it('renders items as real links, so a keyboard can reach them', async () => {
    await mountAdmin();
    const nav = screen.getByRole('navigation', { name: 'Admin navigation' });
    const links = within(nav).getAllByRole('link');
    expect(links.length).toBe(adminNavGroups().reduce((count, group) => count + group.items.length, 0));
    // An `href` is what makes it tabbable, activatable with Enter and openable
    // in a new tab. The reference uses `Box` + `onClick`, which is none of those.
    for (const link of links) expect(link).toHaveAttribute('href');
  });

  it('contributes no heading, so it cannot break the page heading order', async () => {
    /*
     * Caught by axe on the running stack, on all ten pages at once.
     * `MuiTypography`'s override maps the `headingSmall` variant to a real
     * `<h3>`, so the "Elitea Admin" title rendered an h3 above every page's own
     * `<h5>` — a skipped level, and a `heading-order` violation that persistent
     * chrome inflicts on every screen behind it. Pinned here so the next person
     * who reaches for a heading variant in this nav finds out in the unit suite.
     */
    await mountAdmin();
    const nav = screen.getByRole('navigation', { name: 'Admin navigation' });
    expect(within(nav).queryAllByRole('heading')).toEqual([]);
    expect(within(nav).getByText('Elitea Admin')).toBeInTheDocument();
  });

  it('names the collapse control by what it will do', async () => {
    await mountAdmin();
    // Exactly one control carries this name — the logo toggle is named
    // differently on purpose, since two controls sharing a name cannot be told
    // apart by anyone navigating by name.
    expect(screen.getByRole('button', { name: 'Collapse navigation' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Toggle navigation' })).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('admin-nav-collapse-toggle'));
    expect(screen.getByRole('button', { name: 'Expand navigation' })).toBeInTheDocument();
  });

  it('gives every interactive element a visible focus indicator', async () => {
    await mountAdmin();
    const nav = screen.getByRole('navigation', { name: 'Admin navigation' });
    const focusables = [...within(nav).getAllByRole('link'), screen.getByTestId('admin-nav-collapse-toggle')];
    for (const element of focusables) {
      element.focus();
      expect(document.activeElement).toBe(element);
    }
  });
});

/* ── contrast, both schemes ────────────────────────────────────────────── */

/** WCAG relative luminance. */
function luminance(color: string): number {
  const rgba = parseColor(color);
  if (rgba === null) throw new Error(`unparseable colour: ${color}`);
  const channel = (value: number): number => {
    const srgb = value / 255;
    return srgb <= 0.03928 ? srgb / 12.92 : ((srgb + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(rgba.r) + 0.7152 * channel(rgba.g) + 0.0722 * channel(rgba.b);
}

function contrast(a: string, b: string): number {
  const first = luminance(a);
  const second = luminance(b);
  const high = Math.max(first, second);
  const low = Math.min(first, second);
  return (high + 0.05) / (low + 0.05);
}

/** `background.sideBar` is a gradient; both of its stops must clear the bar. */
function backgroundStops(value: string): string[] {
  return value.match(/#[0-9a-fA-F]{3,8}/g) ?? [value];
}

describe('contrast in both schemes', () => {
  for (const scheme of ['light', 'dark'] as const) {
    const tokens: Record<string, string> = DEFAULT_BRAND_PACK.schemes[scheme];
    const stops = backgroundStops(tokens['background.sideBar'] ?? '');

    it(`${scheme}: the inactive item label clears 4.5:1 against the nav background`, () => {
      for (const stop of stops) {
        expect(contrast(tokens['text.metrics'] ?? '', stop)).toBeGreaterThanOrEqual(4.5);
      }
    });

    it(`${scheme}: the ACTIVE item label clears 4.5:1 — colour is not the only cue, but it must still be readable`, () => {
      for (const stop of stops) {
        expect(contrast(tokens['text.secondary'] ?? '', stop)).toBeGreaterThanOrEqual(4.5);
      }
    });

    it(`${scheme}: the focus ring clears 3:1, the non-text bar`, () => {
      for (const stop of stops) {
        expect(contrast(tokens['primary.main'] ?? '', stop)).toBeGreaterThanOrEqual(3);
      }
    });
  }
});

/* ── the footer ────────────────────────────────────────────────────────── */

describe('user footer', () => {
  it('shows the injected operator name', async () => {
    setPermissions(ALL_PERMISSIONS, 'e2e-admin@autotest.local');
    await mountAdmin();
    expect(screen.getByText('e2e-admin@autotest.local')).toBeInTheDocument();
  });

  it('shows the operator initial on the avatar', async () => {
    // Mutation survivors: every index of `name.slice(0, 1)` could be changed —
    // to the empty string, or to the second letter — and nothing noticed. An
    // avatar with no initial is the collapsed rail's only identity cue.
    setPermissions(ALL_PERMISSIONS, 'ops@example.com');
    await mountAdmin();
    expect(within(screen.getByTestId('admin-nav-user-button')).getByText('O')).toBeInTheDocument();
  });

  it('reports the menu state on the button that opens it', async () => {
    // Mutation survivor: `aria-expanded={anchorEl !== null}` inverted tells a
    // screen-reader user the menu is open before they touch it and closed once
    // it is. Invisible to a test that only looks for the Logout item.
    await mountAdmin();
    const button = screen.getByTestId('admin-nav-user-button');
    expect(button).toHaveAttribute('aria-expanded', 'false');

    await userEvent.click(button);
    expect(button).toHaveAttribute('aria-expanded', 'true');
  });

  it('names an operator the handler did not identify by role, not by invention', async () => {
    // `adminUiUserName()`'s fallback chain. Mutating the guard makes the footer
    // render an empty string, which reads as a broken page, or invents an
    // identity — both worse than the generic role word.
    (window as unknown as AdminUiConfigWindow).admin_ui_config = { permissions: ALL_PERMISSIONS };
    await mountAdmin();
    expect(within(screen.getByTestId('admin-nav-user-button')).getByText('Admin')).toBeInTheDocument();
  });

  /*
   * `adminApiBaseUrl()` is pre-existing (unit A14) and this change only added a
   * sibling to its module — but the mutation sweep showed its guard entirely
   * unprotected, and it decides where EVERY admin request goes. An empty
   * `vite_server_url` (which is what the Go handler injects for a session it
   * did not recognise) must fall back to `/api/v2`, not be used as the base:
   * `''` produces same-origin relative URLs that resolve against the current
   * PAGE, so `/admin/app/users` would issue its reads against
   * `/admin/app/admin/auth_users/...`. Four lines to close, so closed here
   * rather than left as a note.
   */
  it('falls back to /api/v2 when the handler injected no API base', () => {
    (window as unknown as AdminUiConfigWindow).admin_ui_config = { permissions: [] };
    expect(adminApiBaseUrl()).toBe('/api/v2');

    (window as unknown as { admin_ui_config?: { vite_server_url?: string } }).admin_ui_config = {
      vite_server_url: '',
    };
    expect(adminApiBaseUrl()).toBe('/api/v2');

    (window as unknown as { admin_ui_config?: { vite_server_url?: string } }).admin_ui_config = {
      vite_server_url: 'https://api.example.test/v2',
    };
    expect(adminApiBaseUrl()).toBe('https://api.example.test/v2');
  });

  it('offers Logout behind the user menu', async () => {
    await mountAdmin();
    expect(screen.queryByTestId('admin-nav-logout')).toBeNull();
    await userEvent.click(screen.getByTestId('admin-nav-user-button'));
    expect(await screen.findByTestId('admin-nav-logout')).toBeInTheDocument();
  });

  it('sweeps the el. namespace on logout', async () => {
    // The `/forward-auth/logout` handoff itself assigns `window.location.href`,
    // which jsdom cannot perform; the observable half here is the storage sweep
    // that `performLogout()` does and the reference's raw `location.href =` did
    // not. The navigation half is covered by the E2E journey.
    await mountAdmin();
    window.localStorage.setItem('el.admin.nav.collapsed', '1');
    window.sessionStorage.setItem('el.something', 'x');
    window.localStorage.setItem('unrelated', 'keep');

    await userEvent.click(screen.getByTestId('admin-nav-user-button'));
    await userEvent.click(await screen.findByTestId('admin-nav-logout'));

    expect(window.localStorage.getItem('el.admin.nav.collapsed')).toBeNull();
    expect(window.sessionStorage.getItem('el.something')).toBeNull();
    expect(window.localStorage.getItem('unrelated')).toBe('keep');
  });
});
