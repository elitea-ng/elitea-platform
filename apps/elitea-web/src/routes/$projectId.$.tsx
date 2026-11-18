import { useEffect, useState } from 'react';

import { createFileRoute } from '@tanstack/react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import type { QueryClient } from '@tanstack/react-query';
import { create } from 'zustand';

import { createHttpClient } from '@/shared/api/http';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { writePersistedProject } from '@/shared/lib/selectedProjectPersistence';

import { NotFoundPage } from './__404';

/**
 * ROUTE-070 (spec §8.1, §0 P10) — the `/:projectId/*` catch-all, ported from
 * `apps/elitea-ui/src/pages/ProjectSwitcher.jsx` (mounted at
 * `[fsd]/app/routes/ProtectedRoutes.jsx:385-388`).
 *
 * FILE NAME / MATCH PRIORITY — verified, not guessed (context7 was
 * unavailable this session, quota exhausted; verification ran directly
 * against the installed `@tanstack/router-generator@1.167.21` and
 * `@tanstack/react-router@1.170.18` source + two scratch scripts):
 *  - The flat file-based convention `$projectId.$.tsx` compiles to route id
 *    `/$projectId/$` — a `$projectId` PARAM segment with a `$` WILDCARD
 *    child (confirmed via `Generator.run()` against a scratch routes dir).
 *  - TanStack Router's matcher (`router-core/src/new-process-route-tree.ts`)
 *    tries segment kinds in a fixed order at every tree node — pathname
 *    (static) → param → optional-param → index → wildcard, LAST — and
 *    backtracks across siblings when a deeper match fails. Neither fact
 *    requires (or has) a manual "priority" option; it falls out of this
 *    route being the only PARAM branch with no sibling WILDCARD to lose to.
 *  - Empirically confirmed with a real `createRouter` + `router.matchRoutes`
 *    (memory history, no DOM): `/chat` (a real static route) resolves to
 *    `/chat`, never this splat (RED/GREEN e). `/foo` and `/29` (single
 *    segment) resolve to `/$projectId/$` (RED/GREEN c). `/artifacts/
 *    edit-bucket` — with a real, empty `/artifacts` route registered and NO
 *    `edit-bucket` child (D4's first anomaly) — BACKTRACKS out of the
 *    `/artifacts` branch into `/$projectId/$` (`projectId="artifacts"`,
 *    splat="edit-bucket"). Bare `/user-public` (D4's second anomaly) matches
 *    the same way. The bare root `/` is NOT matched (out of this route's
 *    scope; R1's index route owns it). See the route-priority describe
 *    block in `__tests__/projectSwitcher.test.tsx` for the executable proof.
 *
 * JUDGMENT CALL — does ROUTE-070 ever "decline"? Re-reading
 * ProjectSwitcher.jsx:15-93 closely: the ROUTER always matches this pattern
 * for any non-empty pathname (proved above) — it never declines at the
 * routing layer. What DOES vary is what the mounted component does once
 * matched: `parseInt(projectId) === project.id` is a STRICT numeric
 * comparison (line 29-31), so for a non-numeric `projectId` (e.g.
 * `"artifacts"` from `/artifacts/edit-bucket`, or the D4 anomalies) the find
 * ALWAYS fails — there is no code path where a non-numeric segment resolves
 * to a real project and triggers the reload. §8.1's own prose ("swallowed
 * by ROUTE-070 → ProjectSwitcher → hard reload against project `artifacts`")
 * is therefore imprecise: the reload only fires when `projectId` parses to
 * an id present in the project list. For the two anomalies that fall inside
 * THIS route (`/artifacts/edit-bucket`, bare `/user-public`) the observable
 * result both apps share is an INLINE 404 render, not a completed reload —
 * D4's "reproduced exactly, bug-for-bug" mandate is satisfied by matching
 * ProjectSwitcher's actual (numeric-only) logic, not its prose gloss. This
 * module reproduces the numeric check byte-for-byte (`findProjectById`
 * below) so the real bug — including this exact edge case — carries over
 * unchanged.
 *
 * PROJECT STATE — zustand per the spec's state decision (replacing
 * `slices/settings.js`'s `setProject`). R2's app-wide bootstrap
 * (`src/app/providers/**`) has not landed, so there is no call site for a
 * real factory yet. `createProjectStore` is the factory (R-S2 compliant —
 * `create()` is called INSIDE a function, never at module scope);
 * `getProjectStore()` is an INTERIM lazy singleton scoped to this route.
 * Consolidation: whichever lands first, R2 (`app/providers`) or E1
 * (`entities/project`), should call `createProjectStore()` once at
 * bootstrap, provide the instance through context/DI, and delete
 * `getProjectStore()` here in favour of the injected instance.
 *
 * PROJECT LIST — S4's generated client (`src/shared/api/generated`, 84
 * operations) has not landed. `fetchProjectList` below is a minimal,
 * hand-written stand-in against the DOCUMENTED endpoint shape (old
 * `apps/elitea-ui/src/api/project.js`: `GET /projects/project/default/
 * {PUBLIC_PROJECT_ID}?check_public_role=true`, response an array of
 * `{id, name, ...}`), built on F4's `createHttpClient` (never raw `fetch` —
 * R-A1). Consolidation: replace with S4's generated `useProjectListQuery`
 * hook once it lands; the query key and response shape are the integration
 * point.
 *
 * ARTIFACTS CACHE RESET (N4 parity) — old code's own comment concedes this
 * is needed "only for Artifacts due to its complex state management"
 * (ProjectSwitcher.jsx:40-41); it is ported without questioning it.
 * `eliteaApi.util.resetApiState()` (RTK Query) fully empties the cache with
 * no side-effect refetch. TanStack Query's equivalent is `queryClient
 * .clear()` (`@tanstack/query-core`'s `QueryClient.clear()` — wipes the
 * query AND mutation caches outright), not `resetQueries()`: `resetQueries`
 * calls `query.reset()` per matched query and then explicitly
 * `refetchQueries({type:'active'})` — firing new network requests a moment
 * before the impending hard `window.location.replace()` is strictly worse
 * than RTK's plain wipe. Verified by reading the installed
 * `@tanstack/query-core` source directly (context7 unavailable this
 * session).
 */

/* ---------------------------------------------------------------------- *
 * Project store — minimal local stand-in, see doc comment above.
 * ---------------------------------------------------------------------- */

interface SelectedProject {
  readonly id: number;
  readonly name: string;
}

interface ProjectStoreState {
  selectedProject: SelectedProject | null;
  setProject: (project: SelectedProject) => void;
}

/**
 * R-S2 factory: `create()` called inside a function, not at module scope.
 * Not exported: `getProjectStore()` below is this file's actual public
 * entry point (the lazy singleton every real caller uses); this factory
 * has no consumer outside it.
 */
function createProjectStore() {
  return create<ProjectStoreState>((set) => ({
    selectedProject: null,
    setProject: (project) => {
      set({ selectedProject: project });
    },
  }));
}

type ProjectStore = ReturnType<typeof createProjectStore>;
let projectStoreInstance: ProjectStore | undefined;

/** Interim lazy singleton — see the "PROJECT STATE" doc comment above. */
export function getProjectStore(): ProjectStore {
  projectStoreInstance ??= createProjectStore();
  return projectStoreInstance;
}

/** Test-only reset, mirrors shared/config/get-config.ts's resetConfigForTests. */
export function resetProjectStoreForTests(): void {
  projectStoreInstance = undefined;
}

/* ---------------------------------------------------------------------- *
 * Project list — minimal local stand-in, see doc comment above.
 * ---------------------------------------------------------------------- */

export interface ProjectListItem {
  readonly id: number;
  readonly name: string;
}

export const PROJECT_LIST_QUERY_KEY = ['project-switcher', 'project-list'] as const;

async function fetchProjectList(): Promise<readonly ProjectListItem[]> {
  const configResult = getConfig();
  if (configResult.status !== 'ok') return [];
  const http = createHttpClient({ baseUrl: configResult.config.vite_server_url });
  const result = await http.get<ProjectListItem[]>(
    `/projects/project/default/${configResult.config.vite_public_project_id}`,
    { query: { check_public_role: true } },
  );
  return result.ok ? result.data : [];
}

/* ---------------------------------------------------------------------- *
 * Pure helpers — independently unit-tested, no DOM/React required.
 * ---------------------------------------------------------------------- */

/** ProjectSwitcher.jsx:29-31 parity: strict numeric id match, nothing else. */
export function findProjectById(
  projectList: readonly ProjectListItem[],
  projectId: string,
): ProjectListItem | undefined {
  const numericId = Number.parseInt(projectId, 10);
  return projectList.find((project) => project.id === numericId);
}

/**
 * ProjectSwitcher.jsx:17-19 parity: `useProjectListQuery(undefined, {skip:
 * !projectId || !parseInt(projectId)})` — the old app never fires the
 * project-list request at all for a non-numeric projectId (every D4 anomaly
 * this splat swallows: `/artifacts/edit-bucket`, bare `/user-public`). Ported
 * as the inverse `enabled` gate, bug-for-bug including the `parseInt(...)`
 * truthiness quirk: a projectId that parses to `0` also stays disabled
 * (`0` is falsy in the old `!parseInt(projectId)` check), even though `0` is
 * never a real project id either way — `findProjectById` above already
 * renders 404 for it regardless of whether the fetch ran.
 */
export function isNumericProjectId(projectId: string): boolean {
  const numericId = Number.parseInt(projectId, 10);
  return projectId !== '' && numericId !== 0 && !Number.isNaN(numericId);
}

/** ProjectSwitcher.jsx:36-37 parity: a plain substring check, not route-aware. */
export function isArtifactsPath(pathname: string): boolean {
  return pathname.includes('/artifacts') || pathname.includes('/create-bucket');
}

/** routes.js:129-131 parity: dev has no basename, production uses vite_base_uri. */
export function getBasename(): string {
  if (import.meta.env.DEV) return '';
  const configResult = getConfig();
  return configResult.status === 'ok' ? configResult.config.vite_base_uri : '';
}

/**
 * ProjectSwitcher.jsx:52-67 parity, byte for byte: strip the FIRST
 * occurrence of `/${projectId}` from the raw browser pathname (which already
 * includes any basename — the OLD app relies on this too), re-prefix the
 * basename only when it is not already present, and reattach search + hash
 * untouched.
 */
export function stripProjectSegment(
  pathname: string,
  projectId: string,
  search: string,
  hash: string,
  basename: string,
): string {
  const pathWithoutProjectId = pathname.replace(`/${projectId}`, '');
  const finalPath =
    basename !== '' && !pathWithoutProjectId.startsWith(basename)
      ? basename + pathWithoutProjectId
      : pathWithoutProjectId;
  return `${finalPath}${search}${hash}`;
}

/* ---------------------------------------------------------------------- *
 * The side effect — DI seam matches src/shared/api/auth/logout.ts's
 * `redirect` pattern (a real default, injectable for tests).
 * ---------------------------------------------------------------------- */

export interface ProjectSwitchParams {
  readonly projectId: string;
  readonly pathname: string;
  readonly search: string;
  readonly hash: string;
}

export interface ProjectSwitchDeps {
  /** Navigation seam; default is a REAL `window.location.replace` (never
   * `.assign`/`.href` — §0 P10 requires the full-page reload semantics of
   * `.replace()`, which also does not add a browser-history entry). */
  replace?: (url: string) => void;
  /** N4 cache-reset seam; only invoked on an artifacts-shaped pathname. */
  resetCache?: () => void;
}

function defaultReplace(url: string): void {
  window.location.replace(url);
}

/**
 * ROUTE-070's side effect once a project IS found. Never a router
 * `navigate()`/`redirect()` — a genuine full-page reload is the point.
 */
export function performProjectSwitch(params: ProjectSwitchParams, deps: ProjectSwitchDeps = {}): void {
  const replace = deps.replace ?? defaultReplace;

  if (isArtifactsPath(params.pathname)) {
    deps.resetCache?.();
  }

  const basename = getBasename();
  const finalPath = stripProjectSegment(params.pathname, params.projectId, params.search, params.hash, basename);
  const baseUrl = `${window.location.protocol}//${window.location.host}`;
  replace(`${baseUrl}${finalPath}`);
}

/* ---------------------------------------------------------------------- *
 * Route + view. The route component is a thin params-forwarding wrapper so
 * the actual logic (`ProjectSwitcherView`) is testable without mounting a
 * full router (it only needs a QueryClientProvider).
 * ---------------------------------------------------------------------- */

/**
 * The route id, exported so tests can build an INDEPENDENT verification
 * router (fresh `createRoute` calls, not this module's `Route` singleton —
 * TanStack Route objects are mutated in place by `.update()`, and sharing
 * one across tests would make them order-dependent) against the exact same
 * path pattern this file ships, instead of a hand-typed duplicate that could
 * drift from it.
 */
export const PROJECT_SPLAT_PATH = '/$projectId/$';

// R1 NOTE (2026-07-27, minimal fix while `src/routes/**` was otherwise
// R3-owned territory, see the unit's final report for the full account):
// `createFileRoute(PROJECT_SPLAT_PATH)` — a named-constant reference —
// built fine under plain `tsc` but broke `Generator.run()` for the WHOLE
// tree with "expected route id to be a string literal or plain template
// literal in /$projectId/$": the generator's route-tree pass parses this
// argument via static AST analysis (to cross-check it against the
// file-derived path) and does not evaluate identifiers, only literals.
// Reproduced against the installed `@tanstack/router-generator@1.168.23`.
// `__root.tsx` landed (this unit), so the `@ts-expect-error` above this
// note was already due to self-destruct per its own comment; inlining the
// literal here resolves both issues in one edit. `PROJECT_SPLAT_PATH`
// stays exported unchanged for its test-router use.
export const Route = createFileRoute('/$projectId/$')({
  component: ProjectSwitcherRoute,
});

function ProjectSwitcherRoute() {
  const { projectId } = Route.useParams();
  return <ProjectSwitcherView projectId={projectId} />;
}

function switchToFoundProject(
  project: ProjectListItem,
  projectId: string,
  queryClient: QueryClient,
): void {
  getProjectStore().getState().setProject({ id: project.id, name: project.name });
  // The selection the app boots from (#2148): `performProjectSwitch` reloads,
  // and the reloaded app reads its selected project from storage. Without this
  // write the project segment of a shared link was stripped and the page opened
  // in whatever project the reader had selected before.
  writePersistedProject({ id: String(project.id), name: project.name });
  performProjectSwitch(
    {
      projectId,
      pathname: window.location.pathname,
      search: window.location.search,
      hash: window.location.hash,
    },
    { resetCache: () => queryClient.clear() },
  );
}

export function ProjectSwitcherView({ projectId }: { projectId: string }) {
  const queryClient = useQueryClient();
  const [projectAvailable, setProjectAvailable] = useState(true);

  const { data: projectList, isLoading } = useQuery({
    queryKey: PROJECT_LIST_QUERY_KEY,
    queryFn: fetchProjectList,
    // ProjectSwitcher.jsx:17-19 parity (see isNumericProjectId's doc comment):
    // no network request at all for a non-numeric projectId. `isFetching`
    // never becomes true while disabled, so `isLoading` (isPending &&
    // isFetching) is false immediately — same "no loading flash" the old
    // app's skip produced, not just the same eventual 404.
    enabled: isNumericProjectId(projectId),
  });

  useEffect(() => {
    if (isLoading) return;
    // `projectList` is undefined in two cases: the query is disabled
    // (non-numeric projectId, never fetches) or — per fetchProjectList's
    // Result discipline (§3.6), which converts every failure into `{ok:
    // false}` resolved to `[]` — this never actually happens once enabled.
    // Either way `?? []` is the correct "no project" fallback.
    const project = findProjectById(projectList ?? [], projectId);
    if (!project) {
      setProjectAvailable(false);
      return;
    }
    switchToFoundProject(project, projectId, queryClient);
  }, [projectId, isLoading, projectList, queryClient]);

  if (!projectAvailable) {
    return <NotFoundPage />;
  }
  return <output>{t('route.projectSwitcher.switching', 'Switching project…')}</output>;
}
