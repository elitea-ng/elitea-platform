import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getListProjectsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { getGetCurrentAuthorMockHandler } from '@/shared/api/generated/social/social.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { BRAND_PACK_GLOBAL, DEFAULT_BRAND_PACK } from '@/shared/brand';
import { installWebStorageShim } from '@/test/webstorage';
import { server } from '@/test/setup';

installWebStorageShim();

import { AppShell } from '../ui/AppShell';
import { useSelectedProjectStore } from '../model/selectedProject.store';
import { useSidebarCollapsedStore } from '@/widgets/sidebar';
import { renderWithNavigation } from './testHarness';

/** Default author fixture: a real `personal_project_id` distinct from every entry `getListProjectsMockHandler` returns below, so R1 tests can tell "personal project" apart from "first project in the list". */
function authorHandler(personalProjectId = '99') {
  return getGetCurrentAuthorMockHandler({
    id: 'user-1',
    name: 'Test User',
    email: 'test@example.com',
    avatar: '',
    description: '',
    personal_project_id: personalProjectId,
  });
}

/** One `ProjectWithGroups` row: the eight keys `internal/api/v2/projects/handler.go` marshals. */
function projectRow(id: number, name: string) {
  return {
    id,
    name,
    owner_id: 1,
    plugins: [],
    keycloak_groups: {},
    create_success: true,
    suspended: false,
    groups: [],
  };
}

/**
 * The project list with project 2 under pylon's reserved personal-project
 * storage name (`project_user_<uid>`).
 *
 * `orderedProjectOptions` substitutes the user-facing "Private" only for a row
 * that still carries that reserved name — an ordinary project name is never
 * overwritten, however `personal_project_id` came to address it — so a test
 * about the substitution has to serve the name the substitution exists for.
 */
function reservedNamePersonalProjectHandler() {
  return getListProjectsMockHandler([projectRow(11, 'Public'), projectRow(2, 'project_user_3')]);
}

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  window.localStorage.clear();
  window.sessionStorage.clear();
  useSelectedProjectStore.setState({ project: null });
  useSidebarCollapsedStore.setState({ collapsed: false });
  server.use(
    // The wire body is ProjectWithGroups: internal/api/v2/projects/handler.go
    // marshals these eight keys and no `status` field.
    getListProjectsMockHandler([
      {
        id: 11,
        name: 'Public',
        owner_id: 1,
        plugins: [],
        keycloak_groups: {},
        create_success: true,
        suspended: false,
        groups: [],
      },
      {
        id: 2,
        name: 'Acme',
        owner_id: 1,
        plugins: [],
        keycloak_groups: {},
        create_success: true,
        suspended: false,
        groups: [],
      },
    ]),
    getPermissionListMockHandler([{ name: 'models.chat.folders.get', enabled: true }]),
    authorHandler(),
  );
});

afterEach(() => {
  vi.unstubAllEnvs();
  resetConfigForTests();
  resetGeneratedClient();
});

describe('AppShell', () => {
  /** The maintenance state `platform_settings` publishes for this caller. */
  function maintenanceHandler(maintenance: {
    enabled: boolean;
    title: string;
    message: string;
    bypass: boolean;
  }) {
    return http.get('*/elitea_core/platform_settings/prompt_lib', () =>
      HttpResponse.json({ chat_enabled: true, maintenance }),
    );
  }

  // A maintenance window replaces the WHOLE shell for a user the API is
  // refusing — sidebar included. Rendering the product around the splash would
  // leave every control looking usable over an API answering 503 to all of
  // them, which is the confusion the splash exists to remove.
  it('replaces the shell with the splash during a maintenance window', async () => {
    server.use(
      maintenanceHandler({
        enabled: true,
        title: 'Scheduled upgrade',
        message: 'Back at 14:00 UTC.',
        bypass: false,
      }),
    );
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );

    expect(await screen.findByTestId('maintenance-splash')).toBeInTheDocument();
    expect(screen.queryByText('page content')).toBeNull();
    expect(screen.queryByTestId('sidebar-create-button')).toBeNull();
  });

  // …and NOT for an administrator, who is the one person who has to be able to
  // reach the admin panel and end the window. `bypass` is resolved by the
  // server from the same permission the middleware admits on; this widget
  // honours it rather than repeating the rule.
  it('leaves the product intact for a caller the server marks as exempt', async () => {
    server.use(
      maintenanceHandler({
        enabled: true,
        title: 'Scheduled upgrade',
        message: 'Back at 14:00 UTC.',
        bypass: true,
      }),
    );
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );

    expect(await screen.findByText('page content')).toBeInTheDocument();
    expect(screen.queryByTestId('maintenance-splash')).toBeNull();
  });

  it('renders the sidebar and the page content together', async () => {
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    expect(screen.getByText('page content')).toBeInTheDocument();
    expect(screen.getByTestId('sidebar-create-button')).toBeInTheDocument();
  });

  // R1 regression: the old app (`settings.js`'s `authorDetails.matchFulfilled`
  // extraReducer) defaults an unselected project to the CALLER'S OWN
  // personal/private project, never to the first entry of the public/shared
  // project list — even when that first entry (`id: 11`, 'Public') would
  // sort ahead of the personal one in `useProjectOptions`' ordering.
  // The default fixture's `personal_project_id` ('99') is deliberately absent
  // from the project list, so this exercises the FALLBACK name specifically.
  it("auto-selects the caller's own personal project, not the first public-pinned project in the list", async () => {
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    await waitFor(() => {
      expect(useSelectedProjectStore.getState().project).toEqual({ id: '99', name: 'Private' });
    });
    expect(window.localStorage.getItem('el.project.id')).toBe('99');
    expect(window.localStorage.getItem('el.project.name')).toBe('Private');
  });

  it("renders the caller's personal project under the user-facing Private name", async () => {
    server.use(reservedNamePersonalProjectHandler(), authorHandler('2'));
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    await waitFor(() => {
      expect(useSelectedProjectStore.getState().project).toEqual({ id: '2', name: 'Private' });
    });
    expect(window.localStorage.getItem('el.project.name')).toBe('Private');
    expect(await screen.findByRole('button', { name: /Project:\s*Private/ })).toBeInTheDocument();
  });

  it('replaces a persisted internal project name with the user-facing name', async () => {
    server.use(reservedNamePersonalProjectHandler(), authorHandler('2'));
    window.localStorage.setItem('el.project.id', '2');
    window.localStorage.setItem('el.project.name', 'project_user_3');
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    await waitFor(() => {
      expect(useSelectedProjectStore.getState().project).toEqual({ id: '2', name: 'Private' });
    });
    expect(window.localStorage.getItem('el.project.name')).toBe('Private');
  });

  /*
   * `resolvePersonalProjectID` (elitea-main's social handler) answers the
   * lowest-id project the caller holds a role in when they have no
   * `project_user_<uid>` project yet, so `personal_project_id` routinely
   * addresses an ORDINARY TEAM PROJECT. Overwriting that project's name with
   * "Private" is what made the switcher, the analytics header and the
   * share-link reload all report the wrong project (journeys 2, 6, 7, 24).
   */
  it("keeps an ordinary project's own name when personal_project_id addresses it", async () => {
    server.use(authorHandler('2'));
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    await waitFor(() => {
      expect(useSelectedProjectStore.getState().project).toEqual({ id: '2', name: 'Acme' });
    });
    expect(window.localStorage.getItem('el.project.name')).toBe('Acme');
    expect(await screen.findByRole('button', { name: /Project:\s*Acme/ })).toBeInTheDocument();
  });

  it('does not auto-select any project until the personal-project signal (GET /social/author) resolves', async () => {
    server.use(authorHandler(''));
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    expect(screen.getByText('page content')).toBeInTheDocument();
    expect(useSelectedProjectStore.getState().project).toBeNull();
  });

  it('sets document.title from the selected (personal) project once one is known', async () => {
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    await waitFor(() => {
      expect(document.title).toContain('Private');
    });
  });

  /**
   * The brand pack's `product.name` must reach `document.title` (JRNY-030's
   * "product name comes from the pack", issue #136 C). Before channel C was
   * wired the product name appeared in exactly one place — the static
   * `<title>Elitea</title>` in `index.html` — and the first route change
   * overwrote it, so nothing observable was ever pack-driven.
   *
   * Asserted against a SERVED pack with a distinct name, not the literal
   * "Elitea": the compiled default carries that same name, so a literal
   * assertion would pass with channel C entirely unwired.
   */
  it("appends the SERVED brand pack's product name to document.title", async () => {
    (window as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL] = {
      ...DEFAULT_BRAND_PACK,
      id: 'autotest-title',
      product: { name: 'Contoso Cloud', shortName: 'Contoso' },
    };

    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );

    // Both halves in ONE waitFor: the product name lands on the first paint
    // while the project is still resolving, so waiting on it alone would
    // resolve early and the project assertion below would race.
    await waitFor(() => {
      expect(document.title).toContain('Contoso Cloud');
      // The project half of the title is preserved, not replaced.
      expect(document.title).toContain('Private');
    });

    delete (window as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL];
  });

  it('prefers a previously-persisted project selection over the auto-picked default', async () => {
    window.localStorage.setItem('el.project.id', '2');
    window.localStorage.setItem('el.project.name', 'Acme');
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    await waitFor(() => {
      expect(useSelectedProjectStore.getState().project).toEqual({ id: '2', name: 'Acme' });
    });
  });

  // R4 regression: old app (`MainSidebar.jsx` line 42 / `MainPanel.jsx` line
  // 19) hides the sidebar entirely and full-bleeds the main content on the
  // `/onboarding` route for a caller with no personal project yet.
  it('hides the sidebar on the onboarding route when the caller has no personal project yet', async () => {
    server.use(authorHandler(''));
    await renderWithNavigation(
      <AppShell>
        <div>onboarding content</div>
      </AppShell>,
      { initialPath: '/onboarding' },
    );
    expect(screen.getByText('onboarding content')).toBeInTheDocument();
    expect(screen.queryByTestId('sidebar-create-button')).not.toBeInTheDocument();
  });

  it('still renders the sidebar on the onboarding route once the caller has a personal project', async () => {
    await renderWithNavigation(
      <AppShell>
        <div>onboarding content</div>
      </AppShell>,
      { initialPath: '/onboarding' },
    );
    await waitFor(() => {
      expect(screen.getByTestId('sidebar-create-button')).toBeInTheDocument();
    });
  });

  it('renders the sidebar (unaffected) on a non-onboarding route regardless of the personal-project signal', async () => {
    server.use(authorHandler(''));
    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );
    expect(screen.getByTestId('sidebar-create-button')).toBeInTheDocument();
  });

  /*
   * DEFECT this pins. On the very first login the browser asks for everything
   * at mount, and `GET /social/author` is the only request that WAITS for
   * provisioning. The project list and the permission list are therefore
   * answered against a user who has no personal project yet, and both come
   * back empty. Nothing re-asked: `staleTime` is 30s and both queries had
   * SUCCEEDED, so the shell opened on "Project: No projects" with every
   * permission-gated nav row missing — Chats, Toolkits & Indexes, MCPs, Credentials,
   * Artifacts — until the user reloaded the page.
   *
   * Both handlers below answer empty ONCE and then the real thing, which is
   * the sequence the server actually produces. A test whose handlers answered
   * the same thing every time could not tell a re-read from a first read.
   */
  it('re-reads the project and permission lists when provisioning lands after them', async () => {
    let projectCalls = 0;
    let permissionCalls = 0;
    server.use(
      http.get('*/projects/project/default/:publicProjectId', () => {
        projectCalls += 1;
        return HttpResponse.json(projectCalls === 1 ? [] : [projectRow(11, 'Public'), projectRow(2, 'Acme')]);
      }),
      http.get('*/auth/permissions/prompt_lib/:projectId', () => {
        permissionCalls += 1;
        return HttpResponse.json(
          permissionCalls === 1
            ? []
            : [
                { name: 'models.chat.folders.get', enabled: true },
                { name: 'models.applications.tools.list', enabled: true },
                { name: 'configuration.artifacts.artifacts.view', enabled: true },
              ],
        );
      }),
      authorHandler('2'),
    );

    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );

    // The switcher names the SELECTED project by finding it in the list, so
    // the stale empty list is what made it read "No projects" beside a
    // perfectly good selection.
    expect(await screen.findByRole('button', { name: /Project:\s*Acme/ })).toBeInTheDocument();
    expect(screen.queryByText('No projects')).not.toBeInTheDocument();

    // The nav rows the empty permission answer had dropped.
    expect(await screen.findByText('Chats')).toBeInTheDocument();
    expect(screen.getByText('Toolkits & Indexes')).toBeInTheDocument();
    expect(screen.getByText('MCPs')).toBeInTheDocument();
    expect(screen.getByText('Credentials')).toBeInTheDocument();
    expect(screen.getByText('Artifacts')).toBeInTheDocument();
  });

  /*
   * DEFECT this pins, and it is the one that shipped. The test above fixes the
   * project list while the AUTHOR query answers correctly at once. On the real
   * first login it does not: the SPA sends two `/social/author` requests at
   * boot, and only the one that starts provisioning waits for it. Measured
   * verbatim on the standalone stack, both inside the same second:
   *
   *   GET /api/v2/social/author/  ->  "personal_project_id":"5"
   *   GET /api/v2/social/author   ->  "personal_project_id":""
   *
   * The shell reads the second. `""` is not an error, so nothing retried, and
   * the auto-select effect — which refuses to select while no project is named
   * — never ran. The settle hook above waited for a selection that could not
   * happen, so neither list was ever re-read: "Project: No projects" and a nav
   * missing every permission-gated row, until the user reloaded the page.
   *
   * The whole sequence is served here, in the order the server produces it.
   */
  it(
    'settles the shell when the first author answer names no project yet',
    { timeout: 30_000 },
    async () => {
      let authorCalls = 0;
      let projectCalls = 0;
      let permissionCalls = 0;
      server.use(
        http.get('*/social/author', () => {
          authorCalls += 1;
          return HttpResponse.json({
            id: 'user-1',
            name: 'Test User',
            email: 'test@example.com',
            avatar: '',
            description: '',
            // Provisioning is still running on the first read, exactly as the
            // server answers it.
            personal_project_id: authorCalls === 1 ? '' : '2',
          });
        }),
        http.get('*/projects/project/default/:publicProjectId', () => {
          projectCalls += 1;
          return HttpResponse.json(projectCalls === 1 ? [] : [projectRow(11, 'Public'), projectRow(2, 'Acme')]);
        }),
        http.get('*/auth/permissions/prompt_lib/:projectId', () => {
          permissionCalls += 1;
          return HttpResponse.json(
            permissionCalls === 1
              ? []
              : [
                  { name: 'models.chat.folders.get', enabled: true },
                  { name: 'models.applications.tools.list', enabled: true },
                  { name: 'configuration.artifacts.artifacts.view', enabled: true },
                ],
          );
        }),
      );

      await renderWithNavigation(
        <AppShell>
          <div>page content</div>
        </AppShell>,
      );

      // The state the user was left in, permanently, before this fix.
      expect(await screen.findByText('No projects')).toBeInTheDocument();

      // …and the state they reach WITHOUT reloading the page.
      expect(await screen.findByRole('button', { name: /Project:\s*Acme/ }, { timeout: 20_000 })).toBeInTheDocument();
      expect(screen.queryByText('No projects')).not.toBeInTheDocument();
      expect(await screen.findByText('Chats', undefined, { timeout: 20_000 })).toBeInTheDocument();
      expect(screen.getByText('Toolkits & Indexes')).toBeInTheDocument();
      expect(screen.getByText('MCPs')).toBeInTheDocument();
      expect(screen.getByText('Credentials')).toBeInTheDocument();
      expect(screen.getByText('Artifacts')).toBeInTheDocument();

      // The author query is what had to be asked again; the two lists follow
      // from its answer.
      expect(authorCalls).toBeGreaterThan(1);
      expect(projectCalls).toBeGreaterThan(1);
    },
  );

  /* The retry is for the race, not for every page load: an ordinary login
     already lists the personal project, and paying two extra requests on each
     of those would be a fix that costs more than the defect. */
  it('asks for neither list a second time when the first answer already carries the personal project', async () => {
    let projectCalls = 0;
    server.use(
      http.get('*/projects/project/default/:publicProjectId', () => {
        projectCalls += 1;
        return HttpResponse.json([projectRow(11, 'Public'), projectRow(2, 'Acme')]);
      }),
      getPermissionListMockHandler([{ name: 'models.chat.folders.get', enabled: true }]),
      authorHandler('2'),
    );

    await renderWithNavigation(
      <AppShell>
        <div>page content</div>
      </AppShell>,
    );

    expect(await screen.findByRole('button', { name: /Project:\s*Acme/ })).toBeInTheDocument();
    await waitFor(() => expect(projectCalls).toBe(1));
  });
});
