import { render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { App } from './App';
import { useSessionStore } from './session-store';
import { server } from '../test/setup';

/**
 * The re-auth popup must open the login path of the plane THIS deployment
 * runs, and App is the only place that knows the plane.
 *
 * DEFECT: `createAuthPopupController` built its popup URL from the module
 * constant `OIDC_LOGIN_PATH`, and App passed no plane at all.
 * `services/elitea-main/internal/api/router.go` registers
 * `/auth/oidc/login` inside `if cfg.OIDCHandler != nil`, so a
 * form-auth deployment has no such route. When the session cookie expired in
 * an open tab, the popup loaded `404 page not found`, `/app/auth-callback`
 * never ran, no result was posted on postMessage, on the BroadcastChannel or
 * in the fallback storage key, and every request behind the single-flight
 * slot stayed pending. The user could not recover the session without a full
 * page reload.
 *
 * The test drives the REAL controller through the real 401 path, so it also
 * covers the wiring: `App` -> `configureGeneratedClient({reauthenticate})` ->
 * `http.ts`'s `runReauth()` -> `window.open`. A test that inspected the
 * controller's options instead would pass on a controller nothing calls.
 *
 * It also pins the LAZY form of the option. The controller is built in a
 * `useState` initializer, which runs before the first `fetchSession()`
 * resolves. A plane read at construction time is always
 * `authPlaneFromProbeStatus(undefined)`, which answers `'oidc'` — the defect,
 * unchanged, on the form plane.
 */

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  resetConfigForTests();
  useSessionStore.setState({ user: undefined, loaded: false, probeStatus: undefined });
});

function configureEnv(): void {
  vi.stubEnv('VITE_SERVER_URL', '/api/v2');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'proj-1');
}

describe('App re-auth popup plane', () => {
  it('opens the Form login path when the probe reports the Form plane', async () => {
    configureEnv();
    // The callback route is exempt from the boot redirect, so the app stays
    // put and jsdom needs no navigation.
    window.history.pushState({}, '', '/auth-callback');

    const opened: string[] = [];
    vi.stubGlobal('open', (url: string) => {
      opened.push(url);
      return { closed: false, close: (): void => {} };
    });

    // 404 on `/auth/info` is what identifies the Form plane: that
    // endpoint is not mounted there at all.
    server.use(
      http.get('/auth/info', () => new HttpResponse(null, { status: 404 })),
      http.get('/api/v2/social/author/', () => new HttpResponse(null, { status: 401 })),
      http.all('*', () => new HttpResponse(null, { status: 401 })),
    );

    render(<App />);
    await waitFor(() => expect(useSessionStore.getState().loaded).toBe(true));
    expect(useSessionStore.getState().probeStatus).toBe(404);

    // A 401 on any generated call is what starts a flight in production.
    // Not awaited: the flight never settles here, which is the point — a
    // popup on a dead-end page settles only when the user closes it.
    void eliteaFetch('/expired', {}).catch(() => undefined);

    await waitFor(() => expect(opened).toHaveLength(1));
    const popupUrl = new URL(opened[0] ?? '', 'http://localhost');
    expect(popupUrl.pathname).toBe('/auth/login');
    // The callback route, correlated by `auth_state`. The base path is empty
    // in this environment; production prefixes it with `/app`.
    expect(popupUrl.searchParams.get('target_to')).toContain('/auth-callback?auth_state=');

    window.history.pushState({}, '', '/');
  });
});
