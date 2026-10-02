/**
 * The project-switch permission refresh in `App.tsx`.
 *
 * DEAD-WIRING GUARD. `refreshPermissions` had store-level coverage
 * (`session-store.test.ts`), but its only production caller — the effect in
 * `App.tsx` — had none. Delete that effect and every other suite stays
 * green, while the route guards judge project B with project A's answers
 * after the first switch.
 *
 * Its own file, not `App.test.tsx`: the session store is a module singleton,
 * and a late continuation from another test in the same file makes the
 * request log ambiguous. `App.redirect.test.tsx`'s header states the same
 * reason.
 */
import { render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';
import { writePersistedProject } from '@/shared/lib/selectedProjectPersistence';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { App } from './App';
import { useSessionStore } from './session-store';
import { server } from '../test/setup';

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  resetConfigForTests();
  useSessionStore.setState({ user: undefined, loaded: false, probeStatus: undefined });
  useSelectedProjectStore.setState({ project: null });
  window.sessionStorage.clear();
  window.localStorage.clear();
});

function configureEnv(): void {
  vi.stubEnv('VITE_SERVER_URL', '/api/v2');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'proj-1');
  // Drop the memo AFTER the stubs land. A late continuation from the
  // previous test can call `getConfig()` once the stubs are gone, which
  // caches a "missing" result for the next test.
  resetConfigForTests();
}

/**
 * Every project id the permission route is asked for, in order.
 *
 * `grantChatTo` names the one project whose list carries
 * `models.chat.folders.get`. Every other project gets an unrelated
 * permission, which is what `requireChatPermission` redirects on.
 */
function capturePermissionReads(grantChatTo = 'proj-9'): string[] {
  const seen: string[] = [];
  server.use(
    http.get('/auth/info', () => HttpResponse.json({ authenticated: true, user_id: 'u-42' })),
    http.get('/api/v2/social/author/', () => HttpResponse.json({ id: 'u-42', personal_project_id: 'proj-9' })),
    http.get('/api/v2/auth/permissions/prompt_lib/:projectId', ({ params }) => {
      const projectId = String(params['projectId']);
      seen.push(projectId);
      const name = projectId === grantChatTo ? 'models.chat.folders.get' : 'models.chat.folders.list';
      return HttpResponse.json([{ name, enabled: true }]);
    }),
    http.all('*', () => new HttpResponse(null, { status: 404 })),
  );
  return seen;
}

describe('App project-switch permission refresh', () => {
  it('re-reads the permission list for the newly selected project', async () => {
    configureEnv();
    vi.stubGlobal('open', () => null);
    const seen = capturePermissionReads();

    render(<App />);

    // Boot reads the list for the personal project.
    await waitFor(() => expect(useSessionStore.getState().loaded).toBe(true));
    await waitFor(() => expect(seen).toEqual(['proj-9']));
    expect(useSessionStore.getState().user?.permissionsProjectId).toBe('proj-9');

    // The two steps `useSelectedProject.selectProject` performs, in its own
    // order: persist first, then publish the selection. `session-store.ts`
    // resolves the project from the persisted value.
    writePersistedProject({ id: 'proj-77', name: 'Team' });
    useSelectedProjectStore.setState({ project: { id: 'proj-77', name: 'Team' } });

    // `permissionsProjectId` is the discriminating assertion: only
    // `refreshPermissions` writes it. The route-level `usePermissionSet`
    // query reads the same URL, but it writes the query cache, not the
    // session store, so it cannot satisfy this on its own.
    await waitFor(() => expect(useSessionStore.getState().user?.permissionsProjectId).toBe('proj-77'));
    expect(seen[0]).toBe('proj-9');
    expect(seen).toContain('proj-77');
  });

  /**
   * The second half of the effect: `router.invalidate()`. A refreshed list
   * alone changes nothing on screen. The guards run in `beforeLoad`, and
   * only an invalidation runs them again.
   *
   * The person opens `/chat` with `models.chat.folders.get`, then switches
   * to a project that does not grant it. `requireChatPermission` must send
   * them to `/onboarding`.
   */
  it('re-runs the route guards after the refresh, so a now-forbidden page redirects', async () => {
    configureEnv();
    vi.stubGlobal('open', () => null);
    capturePermissionReads('proj-9');
    window.history.pushState({}, '', '/chat');

    render(<App />);
    await waitFor(() => expect(useSessionStore.getState().loaded).toBe(true));
    await waitFor(() => expect(window.location.pathname).toBe('/chat'));

    writePersistedProject({ id: 'proj-77', name: 'Team' });
    useSelectedProjectStore.setState({ project: { id: 'proj-77', name: 'Team' } });

    await waitFor(() => expect(window.location.pathname).toBe('/onboarding'));
  });
});
