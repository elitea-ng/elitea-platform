/**
 * The expiry redirect, and the 401s that must NOT cause one.
 *
 * THE DEFECT. A user whose browser session expired while a page was open got
 * no redirect to the identity provider. Every request on the screen failed and
 * the browser stayed where it was. `/forward-auth/info` used to answer
 * `200 {"authenticated": false}` for a cookie that no longer worked, which is
 * the same answer it gives a browser that was never signed in, so nothing
 * could tell the two apart after boot.
 *
 * THE CONTRACT. `/forward-auth/info` now answers `401` with
 * `error.code = "session_expired"` and a `login_url`. The app shell's own
 * session probe navigates on it.
 *
 * THE OTHER HALF, and the reason the handler is per-client rather than global:
 * a 401 from the notification bell, a permission read or any other peripheral
 * call must NOT move the browser. A peripheral 401 once re-authenticated in a
 * loop forever. Both halves are proved here; a rule with only its positive
 * case tested is a rule that will be widened by the next change.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';

import { createHttpClient } from '@/shared/api/http';

import { server } from '../test/setup';

import { createSessionStore } from './session-store';

const INFO = '/forward-auth/info';
const API_BASE = '/api/v2';

/** Exactly what `writeSessionExpired` answers (internal/api/v2/auth/session.go). */
function sessionExpired(loginUrl = '/forward-auth/login?target_to=%2F') {
  return http.get(INFO, () =>
    HttpResponse.json(
      {
        authenticated: false,
        error: { code: 'session_expired', message: 'the browser session is no longer valid' },
        login_url: loginUrl,
      },
      { status: 401, headers: { Location: loginUrl } },
    ),
  );
}

/**
 * jsdom defines `location.assign` as NON-configurable, so `vi.spyOn` on it
 * throws. `location` itself is configurable, so the whole object is replaced
 * for the duration of a test and restored after — the same helper
 * `App.redirect.test.tsx` uses, for the same reason.
 */
const locationRestorers: (() => void)[] = [];

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

afterEach(() => {
  while (locationRestorers.length > 0) locationRestorers.pop()?.();
});

describe('the primary session check redirects on session_expired', () => {
  it('sends the browser to the login start, carrying the page it was on', async () => {
    server.use(sessionExpired());
    const navigate = vi.fn();
    const store = createSessionStore({ apiBaseUrl: API_BASE, navigate });

    await store.getState().fetchSession();

    expect(navigate).toHaveBeenCalledTimes(1);
    const target = new URL(navigate.mock.calls[0]?.[0] as string, window.location.origin);
    expect(target.pathname).toBe('/forward-auth/login');
    // The server cannot know which page the user was on — the probe is an XHR
    // issued from wherever they happen to be — so the client supplies it.
    expect(target.searchParams.get('target_to')).toBe(
      window.location.pathname + window.location.search,
    );
  });

  it('does not redirect when the probe merely says nobody is signed in', async () => {
    server.use(http.get(INFO, () => HttpResponse.json({ authenticated: false })));
    const navigate = vi.fn();
    const store = createSessionStore({ apiBaseUrl: API_BASE, navigate });

    await store.getState().fetchSession();

    // A 200 "not authenticated" is not an expiry: nobody was signed in. The
    // boot effect in App.tsx decides what to do with that, and it is exempt on
    // the callback path and on a probe that never answered.
    expect(navigate).not.toHaveBeenCalled();
  });

  it('does not redirect when the session store is unreachable', async () => {
    server.use(http.get(INFO, () => new HttpResponse(null, { status: 503 })));
    const navigate = vi.fn();
    const store = createSessionStore({ apiBaseUrl: API_BASE, navigate });

    await store.getState().fetchSession();

    // A 503 says nothing about the caller. Signing the user out over a database
    // blip is the failure this status exists to avoid.
    expect(navigate).not.toHaveBeenCalled();
  });
});

describe('a peripheral 401 never moves the browser', () => {
  const PERIPHERAL = '/api/v2/notifications/list';

  it('leaves a client that configured no expiry handler alone', async () => {
    // The SAME body the primary probe navigates on. The only difference is
    // which client received it, and that is the whole rule.
    server.use(
      http.get(PERIPHERAL, () =>
        HttpResponse.json(
          {
            error: { code: 'session_expired', message: 'the browser session is no longer valid' },
            login_url: '/forward-auth/login?target_to=%2F',
          },
          { status: 401, headers: { Location: '/forward-auth/login?target_to=%2F' } },
        ),
      ),
    );
    const navigations: string[] = [];
    stubLocationAssign(navigations);
    // No `onSessionExpired`, and no `reauthenticate`: this is the shape every
    // feature client has.
    const client = createHttpClient({ baseUrl: '/' });

    const result = await client.get(PERIPHERAL);

    expect(result.ok).toBe(false);
    expect(navigations).toEqual([]);
  });

  it('keeps a background poll on the background path', async () => {
    server.use(http.get(PERIPHERAL, () => new HttpResponse(null, { status: 401 })));
    const reauthenticate = vi.fn(() => Promise.resolve());
    const client = createHttpClient({ baseUrl: '/', reauthenticate });

    const result = await client.get(PERIPHERAL, { background: true });

    expect(result.ok).toBe(false);
    // `background: true` is the existing guard against the notification bell's
    // 401 opening a login popup. Adding the expiry rule must not weaken it.
    expect(reauthenticate).not.toHaveBeenCalled();
  });
});
