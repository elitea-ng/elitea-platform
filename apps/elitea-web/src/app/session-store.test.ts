/**
 * The session store is what turns the server-side OIDC cookie into the
 * RouterContext `auth` object every route guard reads, so an undetected
 * regression here logs every user out (or, worse, logs them in as nobody).
 *
 * These tests construct isolated instances via `createSessionStore()` rather
 * than touching the module singleton — that is the reason the factory is
 * exported (same rationale as `createNavBlockerStore`).
 */
import { afterEach, describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';

import { writePersistedProject } from '@/shared/lib/selectedProjectPersistence';

import { server } from '../test/setup';

import { createSessionStore, sessionAuthContext, useSessionStore } from './session-store';

const INFO = '/auth/info';
const API_BASE = '/api/v2';
const AUTHOR = `${API_BASE}/social/author/`;

/** The `/social/author` shape `personal_project_id` really comes from. */
function author(personalProjectId?: string) {
  return http.get(AUTHOR, () =>
    HttpResponse.json({ id: 'u-42', name: 'U', email: 'u@example.test', avatar: '', description: '', ...(personalProjectId !== undefined ? { personal_project_id: personalProjectId } : {}) }),
  );
}

describe('createSessionStore', () => {
  /*
   * The Form plane mounts no /auth/info at all
   * (internal/api/production_router.go mounts one plane or the other), so on a
   * Form deployment the probe 404s. Concluding "not logged in" from that alone
   * sent a browser holding a perfectly good Form session cookie back to the
   * login form on every load — and, with the boot redirect in App.tsx, into a
   * loop. /social/author answers on both planes.
   */
  it('recovers the session from /social/author when /auth/info is absent', async () => {
    server.use(
      http.get(INFO, () => new HttpResponse(null, { status: 404 })),
      author('proj-9'),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user).toMatchObject({ id: 'u-42', personal_project_id: 'proj-9' });
    expect(store.getState().probeStatus).toBe(404);
  });

  it('reports no user when the Form-plane fallback is itself unauthenticated', async () => {
    server.use(
      http.get(INFO, () => new HttpResponse(null, { status: 404 })),
      http.get(AUTHOR, () => new HttpResponse(null, { status: 401 })),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user).toBeUndefined();
    expect(store.getState().loaded).toBe(true);
    expect(store.getState().probeStatus).toBe(404);
  });

  /*
   * A 401 from /auth/info is the OIDC plane saying "no session". It
   * must NOT trigger the Form fallback, and it must keep its own status so the
   * boot redirect picks the OIDC login path rather than the Form one.
   */
  it('does not treat an OIDC 401 as the Form plane', async () => {
    server.use(http.get(INFO, () => new HttpResponse(null, { status: 401 })));
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user).toBeUndefined();
    expect(store.getState().probeStatus).toBe(401);
  });

  it('starts with no user and loaded=false before any probe', () => {
    const store = createSessionStore();
    expect(store.getState().user).toBeUndefined();
    expect(store.getState().loaded).toBe(false);
  });

  it('populates the user from an authenticated /auth/info response', async () => {
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      author('proj-9'),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user).toMatchObject({ id: 'u-42', personal_project_id: 'proj-9' });
    expect(store.getState().loaded).toBe(true);
  });

  /*
   * The #166 regression guard. `personal_project_id` was filled with the USER
   * ID — `/auth/info` returns no project id at all, so the field was
   * fabricated. `NotificationButton` then opened an SSE subscription scoped to
   * a project the user is generally not a member of and was refused with a
   * 403, which EventSource does not retry. This asserts the two values are
   * sourced independently: the store must report the SERVER's project id, and
   * it must never be the user id unless the server said so.
   */
  it('takes personal_project_id from /social/author, never from the user id', async () => {
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      author('7'),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user?.personal_project_id).toBe('7');
    expect(store.getState().user?.personal_project_id).not.toBe('u-42');
  });

  it('leaves personal_project_id absent when the server names no personal project', async () => {
    // An empty string is the server's "no personal project" answer; carrying
    // it through as a truthy-looking value would put `''` in the SSE URL.
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      author(''),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user).toEqual({ id: 'u-42' });
    expect(store.getState().user).not.toHaveProperty('personal_project_id');
  });

  it('still resolves the user when the author probe fails', async () => {
    // A failed author probe must not log the user out — identity comes from
    // /auth/info alone.
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      http.get(AUTHOR, () => new HttpResponse(null, { status: 500 })),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user).toEqual({ id: 'u-42' });
    expect(store.getState().loaded).toBe(true);
  });

  it('does not probe /social/author at all when the session is anonymous', async () => {
    let authorCalls = 0;
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: false })),
      http.get(AUTHOR, () => {
        authorCalls += 1;
        return HttpResponse.json({});
      }),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(authorCalls).toBe(0);
  });

  it('sends the session cookie — the probe is worthless without credentials', async () => {
    let credentialsSeen: string | undefined;
    server.use(
      http.get(INFO, ({ request }) => {
        credentialsSeen = request.credentials;
        return HttpResponse.json({ authenticated: true, user_id: 'u-1' });
      }),
    );
    const store = createSessionStore();
    await store.getState().fetchSession();

    expect(credentialsSeen).not.toBe('omit');
  });

  it('leaves the user undefined when the session is not authenticated', async () => {
    // `user_id` is deliberately present: a body carrying a subject but
    // `authenticated: false` is the shape that proves the `authenticated`
    // check is doing work, rather than the `user_id` check catching it.
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: false, user_id: 'u-9' })),
    );
    const store = createSessionStore();
    await store.getState().fetchSession();

    expect(store.getState().user).toBeUndefined();
    expect(store.getState().loaded).toBe(true);
  });

  it('leaves the user undefined when authenticated=true carries no user_id', async () => {
    // A session with no subject must not produce a user whose id is undefined:
    // the index-route guard would read it as "logged in" and skip the redirect.
    server.use(http.get(INFO, () => HttpResponse.json({ authenticated: true })));
    const store = createSessionStore();
    await store.getState().fetchSession();

    expect(store.getState().user).toBeUndefined();
    expect(store.getState().loaded).toBe(true);
  });

  it('treats a 401 as "not logged in" and still resolves loaded=true', async () => {
    server.use(http.get(INFO, () => new HttpResponse(null, { status: 401 })));
    const store = createSessionStore();
    await store.getState().fetchSession();

    expect(store.getState().user).toBeUndefined();
    expect(store.getState().loaded).toBe(true);
  });

  it('does not reject when the probe fails at the network level', async () => {
    server.use(http.get(INFO, () => HttpResponse.error()));
    const store = createSessionStore();

    // A rejection here would escape App.tsx's mount effect as an unhandled
    // rejection and leave `loaded` false forever, hanging the router.
    await expect(store.getState().fetchSession()).resolves.toBeUndefined();
    expect(store.getState().user).toBeUndefined();
    expect(store.getState().loaded).toBe(true);
  });

  it('gives each instance its own state', async () => {
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-7' })),
    );
    const a = createSessionStore();
    const b = createSessionStore();
    await a.getState().fetchSession();

    expect(a.getState().user?.id).toBe('u-7');
    expect(b.getState().user).toBeUndefined();
  });
});

/**
 * `sessionAuthContext.getSelectedProjectId` is the seam every
 * `useSelectedProjectId` duplicate reads and every project-scoped query is
 * gated on. It returned a hardcoded `undefined`, so those queries never ran
 * and `EditApplication` could not load an agent at all — a deep link
 * cold-loaded to an empty page (JRNY-005).
 */
describe('sessionAuthContext.getSelectedProjectId', () => {
  afterEach(() => {
    localStorage.clear();
    sessionStorage.clear();
    useSessionStore.setState({ user: undefined });
  });

  it('resolves the persisted selected project', () => {
    writePersistedProject({ id: '42', name: 'Default Project' });
    expect(sessionAuthContext.getSelectedProjectId()).toBe('42');
  });

  it('prefers the per-tab session selection over the local one', () => {
    writePersistedProject({ id: '7', name: 'Local' });
    sessionStorage.setItem('el.project.id', '9');
    sessionStorage.setItem('el.project.name', 'This Tab');
    expect(sessionAuthContext.getSelectedProjectId()).toBe('9');
  });

  it('defers with undefined when nothing is selected but a personal project exists', () => {
    useSessionStore.setState({ user: { id: 'u-1', personal_project_id: 'p-1' } });
    expect(sessionAuthContext.getSelectedProjectId()).toBeUndefined();
  });

  it("returns '' when there is no project context at all", () => {
    useSessionStore.setState({ user: { id: 'u-1' } });
    expect(sessionAuthContext.getSelectedProjectId()).toBe('');
  });
});

const PERMISSIONS = `${API_BASE}/auth/permissions/prompt_lib/:projectId`;

/**
 * DEFECT: `AuthUser.permissions` had no writer at all. `routes/-guards/
 * requirePermission.ts` reads it, finds `undefined`, and returns on its
 * "not loaded yet" branch, so `requireChatPermission` and
 * `requireArtifactsPermission` could never redirect. Evidence at the time:
 * `grep -rn "permissions" src/app` matched only the type declaration in
 * `router-context.ts`, and both `set({user: ...})` branches of `fetchSession`
 * wrote `id` and `personal_project_id` only.
 *
 * `GET /auth/permissions/prompt_lib/{projectId}` returns EVERY known
 * permission with an `enabled` flag, so the store must keep the enabled
 * subset. A raw copy of `name` would grant access on a disabled permission.
 */
describe('createSessionStore permissions', () => {
  afterEach(() => {
    localStorage.clear();
    sessionStorage.clear();
  });

  function permissions(...entries: readonly { readonly name: string; readonly enabled: boolean }[]) {
    return http.get(PERMISSIONS, () => HttpResponse.json(entries));
  }

  it('stores the enabled permissions for the personal project', async () => {
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      author('proj-9'),
      permissions(
        { name: 'models.chat.folders.get', enabled: true },
        { name: 'configuration.artifacts.artifacts.view', enabled: false },
      ),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user?.permissions).toEqual(['models.chat.folders.get']);
    expect(store.getState().user?.permissionsProjectId).toBe('proj-9');
  });

  it('reads the list for the SELECTED project, not the personal one', async () => {
    writePersistedProject({ id: 'proj-77', name: 'Team' });
    let askedFor: string | undefined;
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      author('proj-9'),
      http.get(PERMISSIONS, ({ params }) => {
        askedFor = String(params.projectId);
        return HttpResponse.json([{ name: 'models.chat.folders.get', enabled: true }]);
      }),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(askedFor).toBe('proj-77');
    expect(store.getState().user?.permissionsProjectId).toBe('proj-77');
  });

  it('leaves permissions undefined when the list cannot be read', async () => {
    // `undefined` is the "not resolved" answer every guard treats as
    // "do not block". An empty array would mean "no permissions" and would
    // redirect the user away from a page they may well be allowed to open.
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      author('proj-9'),
      http.get(PERMISSIONS, () => new HttpResponse(null, { status: 500 })),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();

    expect(store.getState().user?.permissions).toBeUndefined();
    expect(store.getState().user?.id).toBe('u-42');
  });

  /*
   * A permission list answers for ONE project. After a switch the old list
   * must not judge the new project, so `refreshPermissions` re-reads it.
   */
  it('re-reads the list for the new project on refreshPermissions', async () => {
    server.use(
      http.get(INFO, () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      author('proj-9'),
      http.get(PERMISSIONS, ({ params }) =>
        HttpResponse.json(
          params.projectId === 'proj-77'
            ? [{ name: 'configuration.artifacts.artifacts.view', enabled: true }]
            : [{ name: 'models.chat.folders.get', enabled: true }],
        ),
      ),
    );
    const store = createSessionStore({ apiBaseUrl: API_BASE });
    await store.getState().fetchSession();
    expect(store.getState().user?.permissions).toEqual(['models.chat.folders.get']);

    writePersistedProject({ id: 'proj-77', name: 'Team' });
    await store.getState().refreshPermissions();

    expect(store.getState().user?.permissions).toEqual(['configuration.artifacts.artifacts.view']);
    expect(store.getState().user?.permissionsProjectId).toBe('proj-77');
  });
});
