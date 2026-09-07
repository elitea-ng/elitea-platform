/**
 * Minimal OIDC session store (E2E bootstrap; Wave-3 #60).
 *
 * Calls /forward-auth/info at mount time to read the server-side session
 * cookie and populate the RouterContext's `auth` object.  Replaces the
 * permanent stubAuthContext stub so that the IndexRoute guard can resolve
 * the user and perform the correct redirect.
 *
 * Kept minimal intentionally — only the fields AuthUser requires for the
 * index-route and permission guards: `id` and `personal_project_id`.
 *
 * The store is a zustand slice (§2.3: no Redux); subscribed to by App.tsx
 * which passes a stable AuthContext object to <RouterProvider context=…>.
 *
 * Lazy-singleton factory pattern (R-S2) — see `widgets/app-shell/model/
 * navBlocker.store.ts` and `widgets/sidebar/model/sidebarCollapsed.store.ts`
 * for the rationale: the store is constructed on first use, not at module
 * evaluation, so bootstrap order stays explicit.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { createHttpClient } from '@/shared/api/http';
import { getConfig } from '@/shared/config';
import { readPersistedProject } from '@/shared/lib/selectedProjectPersistence';

import { createProbeClient } from './session-probe-client';

import type { AuthContext, AuthUser } from './router-context';

interface SessionState {
  user: AuthUser | undefined;
  loaded: boolean;
  /**
   * HTTP status of the last `/forward-auth/info` probe, or undefined when the
   * request produced none. It is kept because "no user" alone cannot say WHERE
   * to send the browser: a 404 means that endpoint is not mounted, i.e. the
   * Form plane, and a 401 means the OIDC plane answered "no session". See
   * `shared/api/auth/login-redirect.ts`.
   */
  probeStatus: number | undefined;
  fetchSession: () => Promise<void>;
  /**
   * Re-reads the permission list for the CURRENT project selection.
   *
   * Called by `App.tsx` after a project switch. It clears `permissions`
   * first. A guard that runs while the new list is in flight therefore
   * defers. It does not judge the new project with the old project's answers.
   */
  refreshPermissions: () => Promise<void>;
}

interface SessionInfoResponse {
  authenticated?: boolean;
  user_id?: string;
  email?: string;
}

/**
 * `GET /social/author` — the SAME endpoint the old SPA calls `authorDetails`
 * and reads `personal_project_id` off (`apps/elitea-ui/src/slices/settings.js:
 * 255-262`, which seeds the default project from it, and
 * `widgets/sidebar-root/ui/button/NotificationButton.jsx:18`, which scopes its
 * notification subscription to it). `shared/api/auth/verify-session.ts` already
 * documents this as this stack's `auth/me`-class endpoint.
 */
interface AuthorProfileResponse {
  id?: string;
  personal_project_id?: string;
}

const AUTHOR_PATH = '/social/author/';

/**
 * Reads the caller's personal project id from the API.
 *
 * Built here rather than reusing the module-level `/`-based client because
 * `/social/author` is served by the API base (`vite_server_url`), not by the
 * app origin. `reauthenticate` is deliberately NOT configured: this runs
 * inside the boot probe, where a 401 means "not logged in" — the answer we
 * are asking for — and escalating it into the re-auth popup would loop, the
 * same reasoning `createVerifySession` states for the callback probe.
 *
 * Returns `undefined` on any failure. `undefined` is meaningful: `AuthUser`
 * declares `personal_project_id` optional and consumers gate on it (the
 * notification subscription no-ops, the index guard sends the user to
 * onboarding) — which is the correct behaviour when the server cannot name a
 * personal project, and strictly better than inventing one.
 */
/**
 * The Form plane's session probe.
 *
 * `/forward-auth/info` exists only in the OIDC composition, so on a Form
 * deployment the probe above 404s and can never report a logged-in user — the
 * app would bounce a freshly-authenticated browser straight back to the login
 * form, forever. `/social/author` is the endpoint that CAN answer on both
 * planes: it is authenticated, and it returns the two fields a session needs.
 *
 * A 401 here means "not logged in", which is the answer being asked for, so
 * `reauthenticate` stays unconfigured for the same reason it does below.
 */
async function fetchAuthorSession(
  apiBaseUrl: string | undefined,
): Promise<{ id: string; personalProjectId: string | undefined } | undefined> {
  if (apiBaseUrl === undefined) return undefined;

  const http = createHttpClient({ baseUrl: apiBaseUrl });
  const result = await http.get<AuthorProfileResponse>(AUTHOR_PATH);
  if (!result.ok) return undefined;

  const id = result.data.id;
  if (id === undefined || id === '') return undefined;
  const personalProjectId = result.data.personal_project_id;
  return {
    id,
    personalProjectId: personalProjectId === '' ? undefined : personalProjectId,
  };
}

async function fetchPersonalProjectId(apiBaseUrl: string | undefined): Promise<string | undefined> {
  if (apiBaseUrl === undefined) return undefined;

  const http = createHttpClient({ baseUrl: apiBaseUrl });
  const result = await http.get<AuthorProfileResponse>(AUTHOR_PATH);
  if (!result.ok) return undefined;
  const id = result.data.personal_project_id;
  return id !== undefined && id !== '' ? id : undefined;
}

/**
 * One entry of `GET /auth/permissions/prompt_lib/{projectId}` — a plain array
 * of `{name, enabled}` (see the generated client's own NOTE(W2) on
 * `permissionList`).
 */
interface PermissionEntry {
  name?: string;
  enabled?: boolean;
}

/** `GET /auth/permissions/prompt_lib/{projectId}` — the same route `usePermissionSet` reads. */
function permissionsPath(projectId: string): string {
  return `/auth/permissions/prompt_lib/${encodeURIComponent(projectId)}`;
}

/**
 * The permission names the caller HAS in `projectId`.
 *
 * DEFECT this repairs. `routes/-guards/requirePermission.ts` reads
 * `context.auth.getUser()?.permissions`. It returns early when that value is
 * falsy. Nothing ever wrote the field. `requireChatPermission` and
 * `requireArtifactsPermission` could therefore never redirect. A user without
 * `models.chat.folders.get` opened `/chat` and got an empty conversation rail.
 * The spec §8.1 redirect to `/onboarding` never ran.
 *
 * This function keeps only the `enabled: true` entries. The endpoint returns
 * every known permission with a flag. A raw copy of `name` would grant access
 * on a disabled permission. Two other readers filter the same way:
 * `widgets/sidebar/api/usePermissionSet.ts` and
 * `features/agents/lib/useHasPermission.ts`.
 *
 * This function uses `createHttpClient`, not the generated `permissionList`.
 * The two probes above give the reason. This code runs in the boot sequence.
 * There, a 401 is an answer. The generated client turns a 401 into the
 * re-authentication popup.
 *
 * Returns `undefined` on any failure. `undefined` means "not resolved". That
 * answer keeps the guards on their non-blocking path.
 */
async function fetchEnabledPermissions(
  apiBaseUrl: string | undefined,
  projectId: string | undefined,
): Promise<readonly string[] | undefined> {
  if (apiBaseUrl === undefined || projectId === undefined || projectId === '') return undefined;

  const http = createHttpClient({ baseUrl: apiBaseUrl });
  const result = await http.get<PermissionEntry[]>(permissionsPath(projectId));
  if (!result.ok || !Array.isArray(result.data)) return undefined;

  return result.data
    .filter((entry) => entry.enabled === true && typeof entry.name === 'string' && entry.name !== '')
    .map((entry) => entry.name as string);
}

/** Drops a stale permission answer, so a guard defers instead of judging with it. */
function stripPermissions(user: AuthUser): AuthUser {
  const { permissions: _permissions, permissionsProjectId: _projectId, ...rest } = user;
  return rest;
}

/**
 * The project a permission list is read for: the current selection, or the
 * personal project when nothing is selected yet.
 */
function permissionProjectId(user: AuthUser): string | undefined {
  const persisted = readPersistedProject();
  if (persisted !== null && persisted.id !== '') return persisted.id;
  return user.personal_project_id;
}

/**
 * Adds the resolved permission list to a user. The project id travels with
 * the list: a permission list is per project, and a guard must never judge
 * project B with project A's answers.
 */
async function withPermissions(user: AuthUser, apiBaseUrl: string | undefined): Promise<AuthUser> {
  const projectId = permissionProjectId(user);
  const permissions = await fetchEnabledPermissions(apiBaseUrl, projectId);
  if (permissions === undefined || projectId === undefined) return user;
  return { ...user, permissions, permissionsProjectId: projectId };
}

/**
 * The API base for the author probe. Resolved at CALL time, not at store
 * construction: `getConfig()` reads `window.elitea_ui_config`, which
 * `/app/config.js` installs before the bundle runs, and R-S2 forbids doing
 * that work at module scope.
 */
function resolveApiBaseUrl(): string | undefined {
  const config = getConfig();
  return config.status === 'ok' ? config.config.vite_server_url : undefined;
}

type SessionStore = UseBoundStore<StoreApi<SessionState>>;

/**
 * The session probe deliberately omits `reauthenticate`: a 401 here means
 * "not logged in", which is the answer we are asking for, not a condition to
 * recover from. Escalating it into the re-auth popup would loop.
 *
 * `baseUrl: '/'` — `/forward-auth/info` is served by the same origin as the
 * app, not by the API base.
 */
export interface CreateSessionStoreOptions {
  /**
   * API base for the `/social/author` probe. Defaults to the runtime config's
   * `vite_server_url`. An explicit value exists so a test can exercise the
   * probe without installing a whole runtime config object — the same
   * "inject the boundary, mock nothing else" shape `createVerifySession`
   * already uses for its client.
   */
  readonly apiBaseUrl?: string;
  /**
   * Where an EXPIRED session sends the browser. Defaults to a real
   * navigation; a test replaces it, because jsdom's `window.location.assign`
   * is not configurable.
   */
  readonly navigate?: (url: string) => void;
}

/**
 * The Form-plane user, with its permission list, or `undefined` when
 * `/social/author` cannot name one.
 *
 * Split out of `fetchSession` to keep that function under the §3.5
 * complexity budget.
 */
async function formPlaneUser(apiBaseUrl: string | undefined): Promise<AuthUser | undefined> {
  const author = await fetchAuthorSession(apiBaseUrl);
  if (author === undefined) return undefined;
  const user: AuthUser = {
    id: author.id,
    ...(author.personalProjectId !== undefined ? { personal_project_id: author.personalProjectId } : {}),
  };
  return withPermissions(user, apiBaseUrl);
}


export function createSessionStore(options: CreateSessionStoreOptions = {}): SessionStore {
  const http = createProbeClient(
    options.navigate ??
      ((url: string): void => {
        window.location.assign(url);
      }),
  );

  return create<SessionState>((set, get) => ({
    user: undefined,
    loaded: false,
    probeStatus: undefined,
    fetchSession: async () => {
      const result = await http.get<SessionInfoResponse>('/forward-auth/info');
      // The status lives in a different place on each arm of HttpResult, and
      // the arm that matters here is the FAILURE one: a 404 is what identifies
      // the Form plane. `network` and `aborted` failures carry no status.
      const probeStatus = result.ok
        ? result.status
        : 'status' in result.error
          ? result.error.status
          : undefined;
      if (!result.ok || !result.data.authenticated || !result.data.user_id) {
        // A 404 means the OIDC plane is not mounted, i.e. this is a Form
        // deployment, and the browser may well hold a valid Form session
        // cookie. Ask the endpoint that answers on both planes before
        // concluding "not logged in" — otherwise a logged-in user is sent back
        // to the login form on every load.
        const apiBaseUrl = options.apiBaseUrl ?? resolveApiBaseUrl();
        const fallbackUser = probeStatus === 404 ? await formPlaneUser(apiBaseUrl) : undefined;
        set({ user: fallbackUser, loaded: true, probeStatus });
        return;
      }
      // `/forward-auth/info` (services/elitea-main/internal/api/v2/auth/
      // session.go) returns ONLY authenticated/user_id/email — there is no
      // project id in that response at all. This field used to be filled with
      // the USER id (issue #166), which is not a project id and generally
      // names a project the user is not a member of: `NotificationButton`
      // then opened its SSE subscription against it and was refused with a
      // 403, terminal for EventSource. The real source is `/social/author`.
      const personalProjectId = await fetchPersonalProjectId(options.apiBaseUrl ?? resolveApiBaseUrl());
      const user: AuthUser = {
        id: result.data.user_id,
        ...(personalProjectId !== undefined ? { personal_project_id: personalProjectId } : {}),
      };
      set({
        user: await withPermissions(user, options.apiBaseUrl ?? resolveApiBaseUrl()),
        loaded: true,
        probeStatus,
      });
    },
    refreshPermissions: async () => {
      const current = get().user;
      if (current === undefined) return;
      set({ user: stripPermissions(current) });
      const refreshed = await withPermissions(
        stripPermissions(current),
        options.apiBaseUrl ?? resolveApiBaseUrl(),
      );
      // The session may have been replaced while the request was in flight.
      if (get().user?.id !== current.id) return;
      set({ user: refreshed });
    },
  }));
}

let instance: SessionStore | undefined;

function resolveStore(): SessionStore {
  instance ??= createSessionStore();
  return instance;
}

function useSessionStoreHook<T>(selector: (state: SessionState) => T): T {
  return resolveStore()(selector);
}

/** @public The lazily-constructed singleton, exposed with the hook + getState surface App.tsx uses. */
export const useSessionStore = Object.assign(useSessionStoreHook, {
  getState: (): SessionState => resolveStore().getState(),
  setState: (partial: Partial<SessionState>): void => resolveStore().setState(partial),
});

/**
 * `AuthContext.getSelectedProjectId`, resolving the currently selected
 * project for every consumer of the router context — `pages/agents`,
 * `features/apps` and `features/chat-input` each hold a `useSelectedProjectId`
 * hook that is a one-line read of this seam, and `routes/-guards/`
 * (`skillsGuard`, `integrationGuard`) call it directly.
 *
 * It returned a hardcoded `undefined` — the "R2 integration gap" App.tsx's own
 * header flags ("`<RouterProvider>` does not override the router's stub
 * `context.auth` with a real session-backed one"). Every query gated on the
 * resolved project id was therefore permanently disabled: `EditApplication`
 * never fetched an agent at all, so a deep link to `/agents/:tab/:id/:version`
 * cold-loaded to an empty page with the fallback "Agent" heading (JRNY-005).
 *
 * The selection itself already exists and is already persisted —
 * `widgets/app-shell`'s `useSelectedProject` writes it through
 * `writePersistedProject` on every `selectProject` call, before it updates its
 * own store — so reading the persisted value here is both current and
 * synchronous, which this non-hook seam requires. `app/` is also the only
 * layer permitted to reach into `widgets/` (the three `useSelectedProjectId`
 * duplicates sit in `pages/` and `features/`, which may not import upward).
 *
 * The `undefined` vs `''` branch is the baseline's, per AuthContext's own
 * contract (`router-context.ts`): with no selection but a personal project in
 * play, return `undefined` — "a project exists but is not the active
 * selection yet", which leaves guards on their defer path — and only return
 * `''` when there is no project context at all.
 */
function resolveSelectedProjectId(): string | undefined {
  const persisted = readPersistedProject();
  if (persisted !== null) return persisted.id;
  return useSessionStore.getState().user?.personal_project_id !== undefined ? undefined : '';
}

/** Stable AuthContext backed by the zustand store — passed to RouterProvider. */
export const sessionAuthContext: AuthContext = {
  getUser: () => useSessionStore.getState().user,
  /**
   * `AuthContext.refreshSession` — see that field's contract for why a guard
   * needs it. It re-runs the whole boot probe rather than only re-reading
   * `/social/author`, because the permission list is read for the personal
   * project and a session that learned its project without its permissions
   * would leave `requirePermission` deferring for the rest of the session.
   */
  refreshSession: () => useSessionStore.getState().fetchSession(),
  getSelectedProjectId: resolveSelectedProjectId,
};
