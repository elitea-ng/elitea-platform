import { render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';

import { App } from './App';
import { useSessionStore } from './session-store';
import { server } from '../test/setup';

/**
 * The boot redirect lives in its own file because the session store is a
 * MODULE SINGLETON. Sharing App.test.tsx meant a late continuation from an
 * earlier test navigated through this file's location stub, and the assertion
 * read that navigation instead of the one under test — the failure looked like
 * "wrong auth plane" when the plane logic was right and the state was stale.
 */

/**
 * jsdom defines `location.assign` as NON-configurable, so `vi.spyOn` on it
 * throws "Cannot redefine property". `location` itself is configurable, so the
 * whole object is replaced for the duration of a test and restored after.
 */
function stubLocationAssign(sink: string[]): void {
  const real = window.location;
  Object.defineProperty(window, 'location', {
    configurable: true,
    writable: true,
    value: Object.assign(Object.create(Object.getPrototypeOf(real) as object), real, {
      assign: (url: string | URL) => void sink.push(String(url)),
    }),
  });
  locationRestorers.push(() => {
    Object.defineProperty(window, 'location', { configurable: true, writable: true, value: real });
  });
}

const locationRestorers: (() => void)[] = [];


afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  resetConfigForTests();
  useSessionStore.setState({ user: undefined, loaded: false, probeStatus: undefined });
  while (locationRestorers.length > 0) locationRestorers.pop()?.();
});

function configureEnv(): void {
  vi.stubEnv('VITE_SERVER_URL', '/api/v2');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'proj-1');
}

describe('App boot redirect', () => {
  /*
   * The boot redirect. Nothing gates /app/** at the edge, so an
   * unauthenticated deep link is served the SPA shell; before this, every
   * route guard failed open and the index guard returned {kind:'loading'},
   * leaving a permanent spinner with the login form one redirect away.
   *
   * `assign` is stubbed because jsdom cannot navigate; the assertion is on the
   * URL the app asks for, which is the wiring under test.
   */
  it('sends an unauthenticated browser to the Form login form, carrying it back afterwards', async () => {
    configureEnv();

    const assigned: string[] = [];
    vi.stubGlobal('open', () => null);
    // 404 on the probe = the Form plane; 401 on the author fallback = no
    // session. MSW handlers, not a stubbed fetch: setup.ts runs the server
    // with onUnhandledRequest:'error', which a global stub never reaches.
    server.use(
      http.get('/forward-auth/info', () => new HttpResponse(null, { status: 404 })),
      http.get('/api/v2/social/author/', () => new HttpResponse(null, { status: 401 })),
      http.all('*', () => new HttpResponse(null, { status: 401 })),
    );
    stubLocationAssign(assigned);

    render(<App />);

    // Asserted over the set, not over assigned[0]: the session store is a
    // module singleton, so a late continuation from an earlier test in this
    // file can navigate through the same stub. What must be true is that the
    // boot redirect asked for the FORM login entry point.
    await waitFor(() => {
      const targets = assigned.map((raw) => new URL(raw, 'http://localhost'));
      // Not /forward-auth/auth_form/login: that one 400s without a
      // transaction id, which only /forward-auth/login creates.
      const login = targets.find((url) => url.pathname === '/forward-auth/login');
      expect(login, `no Form login in ${JSON.stringify(assigned)}`).toBeDefined();
      expect(login?.searchParams.get('target_to')).toBe('/');
    });
  });

  it('does not redirect the OIDC callback page, which runs unauthenticated by design', async () => {
    configureEnv();

    window.history.pushState({}, '', '/auth-callback');
    const assigned: string[] = [];
    vi.stubGlobal('open', () => null);
    server.use(
      http.get('/forward-auth/info', () => new HttpResponse(null, { status: 404 })),
      http.all('*', () => new HttpResponse(null, { status: 401 })),
    );
    stubLocationAssign(assigned);

    render(<App />);
    await waitFor(() => expect(useSessionStore.getState().loaded).toBe(true));

    // Redirecting here would cancel the very flight the page exists to finish.
    expect(assigned).toEqual([]);
    window.history.pushState({}, '', '/');
  });

  /*
   * The share link's RECIPIENT.
   *
   * `/shared/chat/<token>` is served to a browser with no session on purpose:
   * its API read is mounted outside every auth middleware, and the page is the
   * only thing a link holder was ever given. The boot redirect took it away
   * from them — the page rendered, the probe came back empty, and the browser
   * left for the identity provider's login screen. Measured as an E2E failure
   * whose call log ends "navigated to .../oauth2/authorize".
   *
   * The path carries the `/app/` basename here, exactly as
   * `window.location.pathname` does in a deployed build, because that is what
   * the predicate must cope with.
   */
  it('does not redirect the shared conversation page, which its reader opens with no account', async () => {
    configureEnv();

    window.history.pushState({}, '', '/app/shared/chat/autotest-share-token');
    const assigned: string[] = [];
    vi.stubGlobal('open', () => null);
    server.use(
      // 200 `authenticated: false` is what the server really answers a browser
      // that presents no cookie at all — see internal/api/v2/auth/session.go.
      http.get('/forward-auth/info', () => HttpResponse.json({ authenticated: false })),
      http.all('*', () => new HttpResponse(null, { status: 401 })),
    );
    stubLocationAssign(assigned);

    render(<App />);
    await waitFor(() => expect(useSessionStore.getState().loaded).toBe(true));

    expect(assigned).toEqual([]);
    window.history.pushState({}, '', '/');
  });

  /*
   * J20c (webkit). A probe that never answered is not a probe that said
   * "no session".
   *
   * The browser cancels every in-flight request the moment it starts leaving a
   * page, and `/forward-auth/info` is issued from App's boot effect. A reload
   * during boot therefore aborted it, and the redirect below then fought the
   * navigation already under way: `page.reload()` failed with "Frame load
   * interrupted" and the browser landed on the OIDC authorize page.
   *
   * `HttpResponse.error()` is MSW's transport failure — the same
   * `kind: 'network'` result an aborted request produces, and the only shape
   * that leaves `probeStatus` undefined.
   */
  it('does not redirect when the session probe never answered', async () => {
    configureEnv();

    const assigned: string[] = [];
    vi.stubGlobal('open', () => null);
    server.use(
      http.get('/forward-auth/info', () => HttpResponse.error()),
      http.all('*', () => new HttpResponse(null, { status: 401 })),
    );
    stubLocationAssign(assigned);

    render(<App />);
    await waitFor(() => expect(useSessionStore.getState().loaded).toBe(true));

    expect(useSessionStore.getState().probeStatus).toBeUndefined();
    expect(assigned).toEqual([]);
  });

});
