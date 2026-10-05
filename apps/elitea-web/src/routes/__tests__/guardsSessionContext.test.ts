/**
 * The permission guards, driven through the REAL `sessionAuthContext`.
 *
 * DEFECT this file exists to catch. `requirePermission` reads
 * `context.auth.getUser()?.permissions` and returns early when it is falsy.
 * Nothing ever wrote that field: the only writer of `state.user` is
 * `createSessionStore().fetchSession`, and both of its success branches set
 * `id` and `personal_project_id` only. So the guard took its pass-through
 * branch on every production evaluation, and `/chat`,
 * `/chat/$conversationId` and `/artifacts` were all unguarded. A user without
 * `models.chat.folders.get` mounted the chat screen with a permanently empty
 * conversation rail instead of the spec §8.1 redirect to `/onboarding`.
 *
 * `guards.test.ts` and `guardsIntegration.test.tsx` both stayed green through
 * all of that, because each builds its own `AuthUser` (or its own `getUser`)
 * carrying `permissions`. They prove the factory works; they cannot see that
 * the app never supplies the input. These cases use the real context object
 * `App.tsx` passes to `<RouterProvider>`, so they fail if the writer is ever
 * removed again.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';

import type { RouterContext } from '@/app/router-context';
import { sessionAuthContext, useSessionStore } from '@/app/session-store';
import { resetConfigForTests } from '@/shared/config/get-config';
import { writePersistedProject } from '@/shared/lib/selectedProjectPersistence';

import { server } from '../../test/setup';

import { requireArtifactsPermission, requireChatPermission } from '../-guards/requirePermission';

const CONTEXT: RouterContext = { auth: sessionAuthContext };

/** `beforeLoad` guards throw the `Response` that `redirect()` returns — see `guards.test.ts`. */
function catchRedirectTarget(run: () => void): string | undefined {
  try {
    run();
  } catch (thrown) {
    return (thrown as { options?: { to?: string } }).options?.to;
  }
  return undefined;
}

describe('permission guards through the real sessionAuthContext', () => {
  afterEach(() => {
    localStorage.clear();
    sessionStorage.clear();
    useSessionStore.setState({ user: undefined });
  });

  it('redirects a user who lacks the chat permission', () => {
    writePersistedProject({ id: 'p-1', name: 'Team' });
    useSessionStore.setState({
      user: { id: 'u-1', permissions: ['configuration.artifacts.artifacts.view'], permissionsProjectId: 'p-1' },
    });

    expect(catchRedirectTarget(() => requireChatPermission({ context: CONTEXT }))).toBe('/onboarding');
  });

  it('lets a user who holds the chat permission through', () => {
    writePersistedProject({ id: 'p-1', name: 'Team' });
    useSessionStore.setState({
      user: { id: 'u-1', permissions: ['models.chat.folders.get'], permissionsProjectId: 'p-1' },
    });

    expect(() => requireChatPermission({ context: CONTEXT })).not.toThrow();
  });

  it('redirects a user who lacks the artifacts permission to /agents', () => {
    writePersistedProject({ id: 'p-1', name: 'Team' });
    useSessionStore.setState({ user: { id: 'u-1', permissions: [], permissionsProjectId: 'p-1' } });

    expect(catchRedirectTarget(() => requireArtifactsPermission({ context: CONTEXT }))).toBe('/agents');
  });

  it('defers while no list has been read yet', () => {
    useSessionStore.setState({ user: { id: 'u-1' } });

    expect(() => requireChatPermission({ context: CONTEXT })).not.toThrow();
  });

  /*
   * A permission list answers for one project. Judging the newly selected
   * project with the previous project's list is a wrong answer, not a slow
   * one, so the guard defers until the refreshed list arrives.
   */
  it('defers when the list belongs to a different project', () => {
    writePersistedProject({ id: 'p-2', name: 'Other' });
    useSessionStore.setState({ user: { id: 'u-1', permissions: [], permissionsProjectId: 'p-1' } });

    expect(() => requireChatPermission({ context: CONTEXT })).not.toThrow();
  });

  it('matches the personal project when nothing is selected yet', () => {
    useSessionStore.setState({
      user: { id: 'u-1', personal_project_id: 'p-9', permissions: [], permissionsProjectId: 'p-9' },
    });

    expect(catchRedirectTarget(() => requireChatPermission({ context: CONTEXT }))).toBe('/onboarding');
  });
});

/**
 * The case the fixture-driven tests above cannot make: no hand-built
 * `AuthUser`. The store performs its real boot sequence against mocked HTTP,
 * and the guard then reads whatever that sequence produced. Before the fix
 * the sequence wrote no `permissions` at all, so the guard could not
 * redirect and this case failed.
 */
describe('the guard after a real session boot', () => {
  const API_BASE = 'https://elitea.example';

  afterEach(() => {
    vi.unstubAllEnvs();
    resetConfigForTests();
    localStorage.clear();
    sessionStorage.clear();
    useSessionStore.setState({ user: undefined, loaded: false });
  });

  function stubValidConfig(): void {
    vi.stubEnv('VITE_SERVER_URL', API_BASE);
    vi.stubEnv('VITE_BASE_URI', '/app/');
    vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
    resetConfigForTests();
  }

  it('redirects to /onboarding for a user the server says has no chat permission', async () => {
    stubValidConfig();
    server.use(
      http.get('/auth/info', () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
      http.get(`${API_BASE}/social/author/`, () => HttpResponse.json({ id: 'u-42', personal_project_id: 'p-9' })),
      http.get(`${API_BASE}/auth/permissions/prompt_lib/:projectId`, () =>
        HttpResponse.json([{ name: 'models.chat.folders.get', enabled: false }]),
      ),
    );

    await useSessionStore.getState().fetchSession();

    expect(catchRedirectTarget(() => requireChatPermission({ context: CONTEXT }))).toBe('/onboarding');
  });
});
