/**
 * ROUTE-070/071 (spec §8.1, §0 P10, §9.3 unit R3) — the highest-risk parity
 * item in the document: `/:projectId/*` swallows every unmatched path
 * (including single-segment ones) and does a HARD `window.location.replace`
 * with the project segment stripped.
 *
 * R-M1 discipline: no `vi.mock()` anywhere in this file. The router, the
 * query client and the zustand store are all real; the only substitutions
 * are the MSW network boundary and a `window.location` override (a DOM
 * global, not an application module — outside R-M1's scope, and the only
 * way to observe `.replace()` at all: jsdom makes `location.replace` a
 * non-configurable own property, so `vi.spyOn` throws "Cannot redefine
 * property" — verified directly against this vitest/jsdom pair before
 * settling on the `Object.defineProperty` override below).
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import type { AnyRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { stubAuthContext } from '../../app/router-context';
import type { RouterContext } from '../../app/router-context';
import { resetConfigForTests } from '../../shared/config/get-config';
import { renderWithTheme } from '../../shared/ui/lib/testTheme';
import { server } from '../../test/setup';
import { routeTree } from '../../routeTree.gen';
import {
  PROJECT_LIST_QUERY_KEY,
  PROJECT_SPLAT_PATH,
  ProjectSwitcherView,
  findProjectById,
  getBasename,
  getProjectStore,
  isArtifactsPath,
  isNumericProjectId,
  performProjectSwitch,
  resetProjectStoreForTests,
  stripProjectSegment,
  type ProjectListItem,
} from '../$projectId.$';
import * as NotFoundModule from '../__404';

const PROJECT_LIST_URL = '*/projects/project/default/*';

const globals = globalThis as unknown as Record<string, unknown>;

function setValidConfig(): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/app/',
    vite_public_project_id: '1',
  };
}

function mockProjectList(projects: readonly ProjectListItem[]): void {
  server.use(http.get(PROJECT_LIST_URL, () => HttpResponse.json(projects)));
}

/** Counts requests to the project-list endpoint — the zero-request proof. */
function mockProjectListWithCounter(projects: readonly ProjectListItem[]): { count: () => number } {
  let calls = 0;
  server.use(
    http.get(PROJECT_LIST_URL, () => {
      calls += 1;
      return HttpResponse.json(projects);
    }),
  );
  return { count: () => calls };
}

/**
 * Full-object override — the only way to make `.replace`/`.assign` spyable
 * (jsdom's `Location` is a class instance, so it is built field-by-field
 * here rather than spread — `elitea`/oxlint's `no-misused-spread` flags
 * spreading a class instance, since it silently drops the prototype).
 */
function overrideLocation(pathname: string, search = '', hash = ''): {
  replaceCalls: string[];
  assignCalls: string[];
} {
  const original = window.location;
  const replaceCalls: string[] = [];
  const assignCalls: string[] = [];
  Object.defineProperty(window, 'location', {
    value: {
      protocol: 'https:',
      host: 'app.example',
      origin: 'https://app.example',
      hostname: 'app.example',
      port: '',
      href: `https://app.example${pathname}${search}${hash}`,
      pathname,
      search,
      hash,
      replace: (url: string) => {
        replaceCalls.push(url);
      },
      assign: (url: string) => {
        assignCalls.push(url);
      },
      reload: () => {},
      toString: () => `https://app.example${pathname}${search}${hash}`,
    },
    writable: true,
    configurable: true,
  });
  restoreLocationOnce = () => {
    Object.defineProperty(window, 'location', { value: original, writable: true, configurable: true });
  };
  return { replaceCalls, assignCalls };
}

let restoreLocationOnce: (() => void) | undefined;

beforeEach(() => {
  resetConfigForTests();
  resetProjectStoreForTests();
  setValidConfig();
});

afterEach(() => {
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetProjectStoreForTests();
  vi.unstubAllEnvs();
  restoreLocationOnce?.();
  restoreLocationOnce = undefined;
});

describe('findProjectById — ProjectSwitcher.jsx:29-31 parity', () => {
  const list: readonly ProjectListItem[] = [
    { id: 29, name: 'Demo' },
    { id: 7, name: 'Other' },
  ];

  it('finds a project whose id matches the parsed projectId', () => {
    expect(findProjectById(list, '29')).toEqual({ id: 29, name: 'Demo' });
  });

  it('returns undefined for an id not in the list', () => {
    expect(findProjectById(list, '999')).toBeUndefined();
  });

  it('returns undefined for a non-numeric projectId — the D4-anomaly case', () => {
    // parseInt('artifacts') is NaN; no project id ever equals NaN.
    expect(findProjectById(list, 'artifacts')).toBeUndefined();
    expect(findProjectById(list, 'user-public')).toBeUndefined();
  });
});

describe('isNumericProjectId — ProjectSwitcher.jsx:17-19 skip-condition parity', () => {
  it.each([
    ['29', true],
    ['1', true],
    ['artifacts', false],
    ['user-public', false],
    ['', false],
    // parseInt('0') is 0, falsy in the old `!parseInt(projectId)` check —
    // reproduced bug-for-bug, not "fixed" to treat '0' as numeric.
    ['0', false],
    ['00', false],
    ['12abc', true], // parseInt('12abc') === 12, exactly like the old app
  ])('isNumericProjectId(%j) === %s', (projectId, expected) => {
    expect(isNumericProjectId(projectId)).toBe(expected);
  });
});

describe('isArtifactsPath — ProjectSwitcher.jsx:36-37 parity', () => {
  it.each([
    ['/29/artifacts', true],
    ['/29/artifacts/bucket-1', true],
    ['/29/create-bucket', true],
    ['/artifacts/edit-bucket', true],
    ['/29/chat', false],
    ['/29', false],
  ])('isArtifactsPath(%s) === %s', (pathname, expected) => {
    expect(isArtifactsPath(pathname)).toBe(expected);
  });
});

describe('getBasename — routes.js:129-131 parity', () => {
  it('is empty in dev regardless of config', () => {
    vi.stubEnv('DEV', true);
    expect(getBasename()).toBe('');
  });

  it('is vite_base_uri outside dev', () => {
    vi.stubEnv('DEV', false);
    expect(getBasename()).toBe('/app/');
  });

  it('falls back to empty when config is missing outside dev', () => {
    vi.stubEnv('DEV', false);
    delete globals['elitea_ui_config'];
    resetConfigForTests();
    expect(getBasename()).toBe('');
  });
});

describe('stripProjectSegment — ProjectSwitcher.jsx:52-67 parity', () => {
  it('strips the project segment and reattaches search + hash', () => {
    expect(stripProjectSegment('/29/artifacts', '29', '?a=1', '#h', '')).toBe('/artifacts?a=1#h');
  });

  it("does not double-prefix the basename when the path already carries it", () => {
    // "/elitea_ui/29/artifacts" -> remove "/29" -> "/elitea_ui/artifacts",
    // which already starts with the basename — comment example from
    // ProjectSwitcher.jsx:56-57, reproduced verbatim.
    expect(stripProjectSegment('/elitea_ui/29/artifacts', '29', '', '', '/elitea_ui')).toBe('/elitea_ui/artifacts');
  });

  it('prefixes the basename when the stripped path does not already carry it', () => {
    expect(stripProjectSegment('/29/artifacts', '29', '', '', '/elitea_ui')).toBe('/elitea_ui/artifacts');
  });

  it('reduces a single-segment path to the empty string (root)', () => {
    expect(stripProjectSegment('/29', '29', '', '', '')).toBe('');
  });
});

describe('performProjectSwitch — RED/GREEN proofs (a), (b), (d)', () => {
  const baseParams = { projectId: '29', pathname: '/29/artifacts', search: '', hash: '' };

  it('[RED/GREEN a] the URL passed to replace has the project segment stripped', () => {
    const replace = vi.fn();
    performProjectSwitch(
      { projectId: '29', pathname: '/29/chat', search: '?x=1', hash: '#y' },
      { replace },
    );
    // jsdom's default location is http://localhost:3000 — protocol/host come
    // straight from `window.location`, exactly like ProjectSwitcher.jsx:52.
    expect(replace).toHaveBeenCalledExactlyOnceWith('http://localhost:3000/chat?x=1#y');
  });

  it('[RED/GREEN b] the DEFAULT deps call window.location.replace, never .assign', () => {
    const { replaceCalls, assignCalls } = overrideLocation('/29/chat', '', '');
    performProjectSwitch({ projectId: '29', pathname: '/29/chat', search: '', hash: '' });
    expect(replaceCalls).toEqual(['https://app.example/chat']);
    expect(assignCalls).toEqual([]);
  });

  it('[RED/GREEN d] resetCache fires for an artifacts-shaped pathname', () => {
    const resetCache = vi.fn();
    performProjectSwitch(baseParams, { replace: vi.fn(), resetCache });
    expect(resetCache).toHaveBeenCalledOnce();
  });

  it('[RED/GREEN d] resetCache does NOT fire for a non-artifacts pathname', () => {
    const resetCache = vi.fn();
    performProjectSwitch({ ...baseParams, pathname: '/29/chat' }, { replace: vi.fn(), resetCache });
    expect(resetCache).not.toHaveBeenCalled();
  });

  it('resetCache is optional — no crash when omitted on an artifacts path', () => {
    expect(() => performProjectSwitch(baseParams, { replace: vi.fn() })).not.toThrow();
  });
});

describe('$projectId/$ route priority — RED/GREEN proofs (c), (e)', () => {
  // Independent verification router: the same path pattern this file's
  // `PROJECT_SPLAT_PATH` compiles to (`/$projectId` param + `/$` wildcard
  // child), built fresh per test rather than reusing this module's `Route`
  // singleton — see PROJECT_SPLAT_PATH's doc comment in `$projectId.$.tsx`.
  it('PROJECT_SPLAT_PATH matches the pattern this suite verifies', () => {
    expect(PROJECT_SPLAT_PATH).toBe('/$projectId/$');
  });

  function buildRouter(initialPath: string) {
    const rootRoute = createRootRoute({ component: Outlet });
    const chatRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat' });
    // Registered but empty — mirrors D4: /artifacts exists, /artifacts/edit-bucket does not.
    const artifactsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/artifacts' });
    const projectIdRoute = createRoute({ getParentRoute: () => rootRoute, path: '/$projectId' });
    const splatRoute = createRoute({ getParentRoute: () => projectIdRoute, path: '/$' });

    const routeTree = rootRoute.addChildren([
      chatRoute,
      artifactsRoute,
      projectIdRoute.addChildren([splatRoute]),
    ]);
    return createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [initialPath] }) });
  }

  // `AnyRouter` (not `ReturnType<typeof buildRouter>`): capturing the router
  // library's own excessively-deep generic through a second function
  // boundary makes TS give up and report an internal `error` pseudo-type on
  // every downstream access — a type-inference limitation, not a real unsafe
  // value (every `it` below asserts on the real, correctly-typed `string`
  // this returns). `AnyRouter` is the library's own documented escape hatch
  // for exactly this shape of helper.
  function leafRouteId(router: AnyRouter, pathname: string): string {
    const matches = router.matchRoutes(pathname, {}) as { routeId?: string }[];
    const last = matches.at(-1);
    if (!last) throw new Error(`no match for ${pathname}`);
    return last.routeId!;
  }

  it('[RED/GREEN e] /chat resolves to the real /chat route, NOT the splat', () => {
    const router = buildRouter('/chat');
    expect(leafRouteId(router, '/chat')).toBe('/chat');
  });

  it('[RED/GREEN c] a single-segment path is swallowed by the splat', () => {
    const router = buildRouter('/foo');
    expect(leafRouteId(router, '/foo')).toBe('/$projectId/$');
  });

  it('a numeric single-segment path (a real project id shape) is also swallowed', () => {
    const router = buildRouter('/29');
    expect(leafRouteId(router, '/29')).toBe('/$projectId/$');
  });

  it('/artifacts itself resolves to the real route, not the splat', () => {
    const router = buildRouter('/artifacts');
    expect(leafRouteId(router, '/artifacts')).toBe('/artifacts');
  });

  it('D4 anomaly: /artifacts/edit-bucket backtracks out of /artifacts into the splat', () => {
    const router = buildRouter('/artifacts/edit-bucket');
    expect(leafRouteId(router, '/artifacts/edit-bucket')).toBe('/$projectId/$');
  });

  it('D4 anomaly: bare /user-public falls into the splat', () => {
    const router = buildRouter('/user-public');
    expect(leafRouteId(router, '/user-public')).toBe('/$projectId/$');
  });

  it('the bare root "/" is NOT matched by the splat (out of ROUTE-070 scope)', () => {
    const router = buildRouter('/');
    const matches = router.matchRoutes('/', {}) as { routeId?: string }[];
    expect(matches.map((m) => m.routeId!)).not.toContain('/$projectId/$');
  });
});

describe('__404.tsx — stays out of the generated route tree (see its doc comment)', () => {
  it('exports NO Route/createFileRoute registration', () => {
    expect('Route' in NotFoundModule).toBe(false);
  });

  it('exports a plain, directly-renderable component', () => {
    renderWithTheme(<NotFoundModule.NotFoundPage />);
    expect(screen.getByRole('main')).toHaveTextContent('Page not found');
  });
});

describe('ProjectSwitcherView — component-level integration', () => {
  function renderView(projectId: string) {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={queryClient}>
        <ProjectSwitcherView projectId={projectId} />
      </QueryClientProvider>,
    );
    return queryClient;
  }

  it('renders NotFoundPage when the runtime config is missing (fetchProjectList short-circuits to [])', async () => {
    delete globals['elitea_ui_config'];
    resetConfigForTests();
    renderView('29');
    await waitFor(() => expect(screen.getByRole('main')).toHaveTextContent('Page not found'));
  });

  it('renders NotFoundPage when the project-list request itself fails (result.ok === false)', async () => {
    server.use(http.get(PROJECT_LIST_URL, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    renderView('29');
    await waitFor(() => expect(screen.getByRole('main')).toHaveTextContent('Page not found'));
  });

  it('renders NotFoundPage when the project id does not resolve (numeric, not in list)', async () => {
    mockProjectList([{ id: 7, name: 'Other' }]);
    renderView('29');
    await waitFor(() => expect(screen.getByRole('main')).toHaveTextContent('Page not found'));
  });

  it('renders NotFoundPage for the D4 anomaly shape (non-numeric projectId)', async () => {
    mockProjectList([{ id: 7, name: 'Other' }]);
    renderView('artifacts');
    await waitFor(() => expect(screen.getByRole('main')).toHaveTextContent('Page not found'));
  });

  it('MEDIUM fix: fires ZERO project-list requests for a non-numeric projectId (ProjectSwitcher.jsx:17-19 skip parity)', async () => {
    const list = mockProjectListWithCounter([{ id: 7, name: 'Other' }]);
    renderView('artifacts');
    // The 404 must still render — and render it via the disabled-query path
    // (isLoading false immediately, no fetch), not via a fetch-then-fail.
    await waitFor(() => expect(screen.getByRole('main')).toHaveTextContent('Page not found'));
    expect(list.count()).toBe(0);
  });

  it('still fires the project-list request for a numeric projectId', async () => {
    const list = mockProjectListWithCounter([{ id: 29, name: 'Demo' }]);
    overrideLocation('/29/chat', '', '');
    vi.stubEnv('DEV', true);
    renderView('29');
    await waitFor(() => expect(list.count()).toBe(1));
  });

  it('sets the project in the store and hard-replaces when the project resolves', async () => {
    mockProjectList([{ id: 29, name: 'Demo' }]);
    const { replaceCalls } = overrideLocation('/29/chat', '?x=1', '');
    vi.stubEnv('DEV', true); // basename empty, keeps the expected URL simple

    renderView('29');

    await waitFor(() => expect(replaceCalls).toEqual(['https://app.example/chat?x=1']));
    expect(getProjectStore().getState().selectedProject).toEqual({ id: 29, name: 'Demo' });
  });

  it('clears the query cache only when switching FROM an artifacts-shaped path', async () => {
    mockProjectList([{ id: 29, name: 'Demo' }]);
    const { replaceCalls } = overrideLocation('/29/artifacts/bucket-1', '', '');
    vi.stubEnv('DEV', true);

    const queryClient = renderView('29');
    const clearSpy = vi.spyOn(queryClient, 'clear');

    await waitFor(() => expect(replaceCalls.length).toBe(1));
    expect(clearSpy).toHaveBeenCalledOnce();
    // The cache genuinely held the project-list query before the clear —
    // proves this is clearing real data, not an always-empty cache.
    expect(clearSpy.mock.instances).toHaveLength(1);
  });

  it('does NOT clear the query cache when switching from a non-artifacts path', async () => {
    mockProjectList([{ id: 29, name: 'Demo' }]);
    const { replaceCalls } = overrideLocation('/29/chat', '', '');
    vi.stubEnv('DEV', true);

    const queryClient = renderView('29');
    const clearSpy = vi.spyOn(queryClient, 'clear');

    await waitFor(() => expect(replaceCalls.length).toBe(1));
    expect(clearSpy).not.toHaveBeenCalled();
  });

  it('shows a switching indicator while the project list is loading', () => {
    mockProjectList([{ id: 29, name: 'Demo' }]);
    overrideLocation('/29/chat', '', '');
    renderView('29');
    expect(screen.getByRole('status')).toHaveTextContent('Switching project');
  });
});

describe('ProjectSwitcherRoute — end-to-end through R1s REAL generated router', () => {
  // Covers the thin params-forwarding wrapper itself (`Route.useParams()` ->
  // `ProjectSwitcherView`), which needs a genuine `RouterProvider` ancestor —
  // everything else in this file deliberately avoids that requirement (see
  // `ProjectSwitcherView`'s doc comment). This mounts R1's ACTUAL generated
  // `routeTree` (`src/routeTree.gen.ts`, landed mid-way through this unit —
  // see `PROJECT_SPLAT_PATH`'s doc comment in `$projectId.$.tsx`) rather than
  // a hand-built stand-in: the strongest possible proof that this file's
  // route is wired correctly into the real app tree, not just a shape that
  // resembles it. `context`/`RouterContext` mirror `app/router.tsx`'s own
  // construction exactly (`createRootRouteWithContext<RouterContext>()`
  // requires it).
  it('forwards the URL param through to ProjectSwitcherView and hard-replaces', async () => {
    mockProjectList([{ id: 29, name: 'Demo' }]);
    const { replaceCalls } = overrideLocation('/29/chat', '', '');
    vi.stubEnv('DEV', true);

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const router = createRouter({
      routeTree,
      context: { auth: stubAuthContext } satisfies RouterContext,
      history: createMemoryHistory({ initialEntries: ['/29/chat'] }),
    });

    render(
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );

    await waitFor(() => expect(replaceCalls).toEqual(['https://app.example/chat']));
  });
});

describe('getProjectStore — R-S2 factory shape', () => {
  it('is a lazy singleton: repeated calls return the same store', () => {
    expect(getProjectStore()).toBe(getProjectStore());
  });

  it('resetProjectStoreForTests forces a fresh instance', () => {
    const first = getProjectStore();
    resetProjectStoreForTests();
    expect(getProjectStore()).not.toBe(first);
  });

  it('starts with no selected project', () => {
    expect(getProjectStore().getState().selectedProject).toBeNull();
  });
});

describe('PROJECT_LIST_QUERY_KEY — the cache-reset special case actually holds real data', () => {
  it('is populated in the query cache once the view has fetched the project list', async () => {
    mockProjectList([{ id: 29, name: 'Demo' }]);
    overrideLocation('/29/chat', '', '');
    vi.stubEnv('DEV', true);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectSwitcherView projectId="29" />
      </QueryClientProvider>,
    );

    await waitFor(() => expect(queryClient.getQueryData(PROJECT_LIST_QUERY_KEY)).toBeDefined());
    expect(queryClient.getQueryData(PROJECT_LIST_QUERY_KEY)).toEqual([{ id: 29, name: 'Demo' }]);
  });
});
