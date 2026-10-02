/**
 * Auth popup controller — §5.4 behaviour 4 (crypto.randomUUID state, verified
 * on delivery) + the controller-level single-flight and delivery channels.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { installWebStorageShim } from '../../../test/webstorage';

installWebStorageShim();

import { sendAuthResult } from './callback';
import type { AuthChannelLike } from './channel';
import { AUTH_MESSAGE_TYPE } from './constants';
import { AuthPopupError, createAuthPopupController } from './popup';
import type { AuthPopupOptions, PopupWindowLike } from './popup';

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

class FakeChannel implements AuthChannelLike {
  onmessage: ((event: { data: unknown }) => void) | null = null;
  readonly sent: unknown[] = [];
  readonly name: string;
  closed = false;
  constructor(name: string) {
    this.name = name;
  }
  postMessage(data: unknown): void {
    this.sent.push(data);
  }
  close(): void {
    this.closed = true;
  }
}

interface Harness {
  controller: ReturnType<typeof createAuthPopupController>;
  openedUrls: string[];
  openedNames: string[];
  openedFeatures: string[];
  channels: FakeChannel[];
  popup: PopupWindowLike & { closed: boolean };
  stateOf(index?: number): string;
}

function harness(overrides: Partial<AuthPopupOptions> = {}): Harness {
  const openedUrls: string[] = [];
  const openedNames: string[] = [];
  const openedFeatures: string[] = [];
  const channels: FakeChannel[] = [];
  // A real window reports `closed` again after `close()`, and a freshly
  // opened one reports `closed === false`. The fake obeys both rules: the
  // controller now reads `closed` to decide when the previous popup is gone.
  const popup = {
    closed: false,
    close(): void {
      popup.closed = true;
    },
  };
  const controller = createAuthPopupController({
    openWindow: (url, name, features) => {
      openedUrls.push(url);
      openedNames.push(name);
      openedFeatures.push(features);
      popup.closed = false;
      return popup;
    },
    createChannel: (name) => {
      const channel = new FakeChannel(name);
      channels.push(channel);
      return channel;
    },
    ...overrides,
  });
  return {
    controller,
    openedUrls,
    openedNames,
    openedFeatures,
    channels,
    popup,
    /**
     * The popup opens the OIDC login endpoint, so the correlated
     * `auth_state` lives one level in — inside the `target_to` that names
     * this app's callback route. See `constants.ts`'s `SSO_LOGIN_PATH` for
     * why the popup cannot open the callback route directly.
     */
    stateOf(index = 0) {
      const url = openedUrls[index];
      if (url === undefined) throw new Error(`no popup ${index} opened`);
      const target = new URL(url).searchParams.get('target_to');
      if (target === null) throw new Error('no target_to in popup URL');
      const state = new URLSearchParams(target.slice(target.indexOf('?'))).get('auth_state');
      if (state === null) throw new Error('no auth_state in popup target_to');
      return state;
    },
  };
}

function resultMessage(state: string, success = true): unknown {
  return { type: AUTH_MESSAGE_TYPE, state, success };
}

async function settled(promise: Promise<unknown>): Promise<boolean> {
  let done = false;
  const flagged = promise.then(
    () => {
      done = true;
    },
    () => {
      done = true;
    },
  );
  await Promise.race([flagged, new Promise((r) => setTimeout(r, 5))]);
  return done;
}

afterEach(() => {
  vi.useRealTimers();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe('behaviour 4 — crypto.randomUUID state', () => {
  it('uses a UUID for the popup state and persists it under el.auth.state', async () => {
    const h = harness();
    const flight = h.controller.reauthenticate();
    const state = h.stateOf();
    expect(state).toMatch(UUID_V4); // the old Math.random+Date state can never match
    expect(window.sessionStorage.getItem('el.auth.state')).toBe(state);

    h.channels[0]?.onmessage?.({ data: resultMessage(state) });
    await flight;
  });

  it('generates a distinct state per flight', async () => {
    const h = harness();
    const first = h.controller.reauthenticate();
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf(0)) });
    await first;
    const second = h.controller.reauthenticate();
    h.channels[1]?.onmessage?.({ data: resultMessage(h.stateOf(1)) });
    await second;
    expect(h.stateOf(0)).not.toBe(h.stateOf(1));
  });

  /**
   * The popup opens the OIDC LOGIN endpoint with the callback route
   * (`baseOrigin + basePath + ROUTE-001 + ?auth_state=`) as its `target_to`,
   * NOT the callback route itself. Opening the callback route directly can
   * never re-authenticate on a stack that does not gate the SPA at the edge —
   * the popup is simply served the app, its session probe reports "no
   * session", and the flight rejects. Measured on the E2E stack; see
   * `constants.ts`'s `SSO_LOGIN_PATH`.
   *
   * Asserted as an exact URL, and the `target_to` is decoded and compared in
   * full: a percent-encoding slip here (or a `/app//auth-callback` double
   * slash from a `VITE_BASE_URI` that ends in `/`) produces a target that
   * elitea-main accepts and redirects to but that matches no route, which
   * would strand every flight in `popup_closed` with nothing to point at.
   */
  it('opens the sign-in page with the callback URL as target_to (ROUTE-001)', async () => {
    const h = harness({ baseOrigin: 'https://backend.example', basePath: '/elitea_ui' });
    const flight = h.controller.reauthenticate();
    const url = h.openedUrls[0] ?? '';
    const target = `/elitea_ui/auth-callback?auth_state=${h.stateOf()}`;
    expect(url).toBe(
      `https://backend.example/forward-auth/login?target_to=${encodeURIComponent(target)}`,
    );
    expect(new URL(url).searchParams.get('target_to')).toBe(target);
    expect(h.openedFeatures[0]).toContain('width=500,height=600'); // clamped minimums
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf()) });
    await flight;
  });

  /**
   * DEFECT: the popup URL was built from the module constant
   * `OIDC_LOGIN_PATH` with no plane input at all.
   * `services/elitea-main/internal/api/router.go` registers
   * `/forward-auth/auth_oidc/login` inside `if cfg.OIDCHandler != nil`, so a
   * form-auth deployment does not have that route. The popup loaded
   * `404 page not found`. `/app/auth-callback` never ran. No result reached
   * the opener on postMessage, on the BroadcastChannel or in the fallback
   * key. Every request behind the single-flight slot stayed pending. The
   * flight settled only when the user noticed the window and closed it, and
   * then it rejected `popup_closed`. Session recovery was impossible.
   *
   * `/forward-auth/login` is the form plane's own entry point: it opens a
   * login transaction (`browserauth.beginLogin`) and
   * `browserflow.CanonicalReturnTarget` preserves the `?auth_state=` query,
   * so the flight completes there.
   */
  it('opens the login path the caller supplies, so the form plane works', async () => {
    const h = harness({
      baseOrigin: 'https://backend.example',
      basePath: '/app',
      loginPath: '/forward-auth/login',
    });
    const flight = h.controller.reauthenticate();
    const url = h.openedUrls[0] ?? '';
    const target = `/app/auth-callback?auth_state=${h.stateOf()}`;
    expect(url).toBe(
      `https://backend.example/forward-auth/login?target_to=${encodeURIComponent(target)}`,
    );
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf()) });
    await flight;
  });

  /**
   * The plane is read from the `/forward-auth/info` probe, and the controller
   * is built before the first probe answers. A login path resolved at
   * construction time is therefore always the OIDC default on both planes,
   * which is the defect above. The getter must run per flight.
   */
  it('reads the login path once per flight, not once per controller', async () => {
    let plane = 'oidc';
    const h = harness({
      baseOrigin: 'https://backend.example',
      basePath: '/app',
      loginPath: () =>
        plane === 'form' ? '/forward-auth/login' : '/forward-auth/auth_oidc/login',
    });

    // The probe has not answered yet: the first flight uses the default.
    const first = h.controller.reauthenticate();
    expect(h.openedUrls[0]).toContain('/forward-auth/auth_oidc/login');
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf(0)) });
    await first;

    // The probe answered 404, so the app now knows it is the form plane.
    plane = 'form';
    const second = h.controller.reauthenticate();
    expect(h.openedUrls[1]).toContain('/forward-auth/login');
    expect(h.openedUrls[1]).not.toContain('/auth_oidc/');
    h.channels[1]?.onmessage?.({ data: resultMessage(h.stateOf(1)) });
    await second;
  });
});

describe('state verification on delivery', () => {
  it('ignores results carrying the wrong state, then accepts the right one', async () => {
    const h = harness();
    const flight = h.controller.reauthenticate();
    h.channels[0]?.onmessage?.({ data: resultMessage('11111111-2222-4333-8444-555555555555') });
    expect(await settled(flight)).toBe(false); // still pending — wrong state ignored
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf()) });
    await flight;
  });

  it('ignores malformed messages', async () => {
    const h = harness();
    const flight = h.controller.reauthenticate();
    h.channels[0]?.onmessage?.({ data: { type: 'something-else' } });
    h.channels[0]?.onmessage?.({ data: null });
    expect(await settled(flight)).toBe(false);
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf()) });
    await flight;
  });

  it('rejects with auth_failed on a success:false result', async () => {
    const h = harness();
    const flight = h.controller.reauthenticate();
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf(), false) });
    await expect(flight).rejects.toMatchObject({ name: 'AuthPopupError', reason: 'auth_failed' });
  });
});

describe('delivery channels', () => {
  it('accepts a window postMessage from the page origin only', async () => {
    const h = harness();
    const flight = h.controller.reauthenticate();
    window.dispatchEvent(
      new MessageEvent('message', { data: resultMessage(h.stateOf()), origin: 'https://evil.example' }),
    );
    expect(await settled(flight)).toBe(false);
    window.dispatchEvent(
      new MessageEvent('message', { data: resultMessage(h.stateOf()), origin: window.location.origin }),
    );
    await flight;
  });

  it('scopes the BroadcastChannel to the flight state', async () => {
    const h = harness();
    const flight = h.controller.reauthenticate();
    expect(h.channels[0]?.name).toBe(`elitea-auth-${h.stateOf()}`);
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf()) });
    await flight;
    expect(h.channels[0]?.closed).toBe(true);
  });

  it('consumes the localStorage fallback via the poll and removes the key', async () => {
    vi.useFakeTimers();
    const h = harness();
    const flight = h.controller.reauthenticate();
    const key = `el.auth.result.${h.stateOf()}`;
    window.localStorage.setItem(key, JSON.stringify(resultMessage(h.stateOf())));
    await vi.advanceTimersByTimeAsync(300);
    await flight;
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  it('consumes the localStorage fallback on a storage event without waiting for the poll', async () => {
    const h = harness();
    const flight = h.controller.reauthenticate();
    const key = `el.auth.result.${h.stateOf()}`;
    window.localStorage.setItem(key, JSON.stringify(resultMessage(h.stateOf())));
    window.dispatchEvent(new StorageEvent('storage', { key }));
    await flight;
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  it('two concurrent controllers cannot consume each other’s fallback result', async () => {
    // Two tabs mid-re-auth. With ONE shared fallback key the loser reads the
    // winner's result, discards it on state mismatch AND deletes it — so the
    // rightful owner hangs to popup_closed. The key is state-scoped instead.
    const tabA = harness();
    const flightA = tabA.controller.reauthenticate();
    // A second TAB owns a separate sessionStorage. Clearing it here is what
    // makes these two controllers two TABS rather than two documents of one
    // tab, which would adopt one another's flight (issue #364).
    window.sessionStorage.clear();
    const tabB = harness();
    const flightB = tabB.controller.reauthenticate();
    expect(tabA.stateOf()).not.toBe(tabB.stateOf());

    // B's callback page reports its result through the REAL writer.
    sendAuthResult(
      { type: AUTH_MESSAGE_TYPE, state: tabB.stateOf(), success: true },
      { opener: null, createChannel: () => null }, // force the localStorage path
    );
    window.dispatchEvent(new StorageEvent('storage', { key: `el.auth.result.${tabB.stateOf()}` }));

    await expect(flightB).resolves.toBeUndefined();
    expect(await settled(flightA)).toBe(false); // A untouched, still waiting
    expect(window.localStorage.getItem(`el.auth.result.${tabB.stateOf()}`)).toBeNull();

    // A then completes on its own state, proving it was never poisoned.
    tabA.channels[0]?.onmessage?.({ data: resultMessage(tabA.stateOf()) });
    await expect(flightA).resolves.toBeUndefined();
  });
});

describe('single-flight + lifecycle', () => {
  it('returns the SAME promise while a flight is pending and opens one popup', async () => {
    const h = harness();
    const first = h.controller.reauthenticate();
    const second = h.controller.reauthenticate();
    expect(second).toBe(first);
    expect(h.controller.pending).toBe(true);
    expect(h.openedUrls).toHaveLength(1);
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf()) });
    await first;
    expect(h.controller.pending).toBe(false);
  });

  it('cleans up state storage and starts a fresh flight after settle', async () => {
    const h = harness();
    const first = h.controller.reauthenticate();
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf(0)) });
    await first;
    expect(window.sessionStorage.getItem('el.auth.state')).toBeNull();
    const second = h.controller.reauthenticate();
    expect(h.openedUrls).toHaveLength(2);
    h.channels[1]?.onmessage?.({ data: resultMessage(h.stateOf(1)) });
    await second;
  });

  it('rejects popup_blocked when the popup cannot open (default window.open + BroadcastChannel path)', async () => {
    const openSpy = vi.spyOn(window, 'open').mockReturnValue(null); // browser-API spy, not an app-module mock
    const controller = createAuthPopupController(); // real defaults on purpose
    await expect(controller.reauthenticate()).rejects.toMatchObject({ reason: 'popup_blocked' });
    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(window.sessionStorage.getItem('el.auth.state')).toBeNull();
    // A blocked popup must NOT poison the controller: the flight slot has to
    // be empty again. Re-auth fires from a background 401, i.e. without user
    // activation — precisely when browsers block window.open — so a stuck
    // slot would kill re-auth for the whole page lifetime.
    expect(controller.pending).toBe(false);
    openSpy.mockRestore();
  });

  it('a blocked popup does not poison the controller — the NEXT call opens a window again', async () => {
    const h = harness({
      openWindow: (url, _name, features) => {
        h.openedUrls.push(url);
        h.openedFeatures.push(features);
        return null; // browser blocks it: no user activation behind a background 401
      },
    });
    await expect(h.controller.reauthenticate()).rejects.toMatchObject({ reason: 'popup_blocked' });
    expect(h.controller.pending).toBe(false);

    // Still blocked: every attempt must reach window.open rather than
    // short-circuit on a stale rejected promise parked in the flight slot.
    await expect(h.controller.reauthenticate()).rejects.toMatchObject({ reason: 'popup_blocked' });
    await expect(h.controller.reauthenticate()).rejects.toMatchObject({ reason: 'popup_blocked' });
    expect(h.openedUrls).toHaveLength(3);
  });

  it('recovers once the user unblocks popups — a later flight opens and succeeds', async () => {
    let blocked = true;
    const h = harness({
      openWindow: (url, _name, features) => {
        h.openedUrls.push(url);
        h.openedFeatures.push(features);
        return blocked ? null : h.popup;
      },
    });
    await expect(h.controller.reauthenticate()).rejects.toMatchObject({ reason: 'popup_blocked' });

    blocked = false; // user allows popups for this site
    const flight = h.controller.reauthenticate();
    expect(h.openedUrls).toHaveLength(2);
    expect(h.controller.pending).toBe(true);
    h.channels[1]?.onmessage?.({ data: resultMessage(h.stateOf(1)) }); // flight 2's own channel
    await expect(flight).resolves.toBeUndefined();
    expect(h.controller.pending).toBe(false);
  });

  it('rejects popup_closed when the user closes the popup', async () => {
    vi.useFakeTimers();
    const h = harness();
    const flight = h.controller.reauthenticate();
    h.popup.closed = true;
    const expectation = expect(flight).rejects.toBeInstanceOf(AuthPopupError);
    // One `closed` reading is not proof (issue #364). The controller believes
    // the reading only after it holds for the full confirmation window.
    await vi.advanceTimersByTimeAsync(1500);
    await expectation;
    expect(h.controller.pending).toBe(false);
  });
});

/**
 * Issue #364 — a second re-auth flight must never re-navigate a live popup.
 *
 * Measured on a WebKit Playwright trace of J3
 * (`e2e/journeys/shell/shell.session.spec.ts:23`): the app opened the popup,
 * then 0.8 s later drove a SECOND `/forward-auth/auth_oidc/login` hop, with a
 * different `auth_state`, into the SAME popup page. The re-navigation landed
 * between the fill and the click, so the user lost the typed value and the
 * form submitted empty.
 *
 * Two independent defects produce that outcome, and each test below pins one.
 */
describe('issue 364 — one popup per user, never re-navigated', () => {
  it('keeps the guard through a transient `closed` reading during navigation', async () => {
    vi.useFakeTimers();
    const h = harness();
    const first = h.controller.reauthenticate();
    expect(h.openedUrls).toHaveLength(1);

    let firstSettled = false;
    const mark = (): void => {
      firstSettled = true;
    };
    void first.then(mark, mark);

    // WebKit reports `closed` as true for a cross-origin popup WHILE that
    // popup crosses from the app origin to the provider origin. The popup is
    // alive. The user types in it.
    h.popup.closed = true;
    await vi.advanceTimersByTimeAsync(300);
    // The popup navigated, so it was never closed. A closed window cannot
    // navigate, which proves the reading above was false.
    h.popup.closed = false;
    await vi.advanceTimersByTimeAsync(300);
    expect(firstSettled).toBe(false);

    // A second 401 arrives while the popup is live.
    const second = h.controller.reauthenticate();
    await vi.advanceTimersByTimeAsync(300);
    expect(second).toBe(first); // it joins flight one...
    expect(h.openedUrls).toHaveLength(1); // ...and opens no second window.

    // The user finishes and authorizes. Flight one completes as normal.
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf()) });
    await expect(first).resolves.toBeUndefined();
  });

  it('opens no second popup while the popup of the previous flight is live', async () => {
    vi.useFakeTimers();
    // This popup ignores `close()`, which a popup that lost `window.opener`
    // does: `routes/auth-callback.tsx` closes itself ONLY when an opener is
    // present. The guard must still hold until the window is really gone.
    const popups: Array<{ closed: boolean; close: () => void }> = [];
    const h: Harness = harness({
      openWindow: (url, name, features) => {
        h.openedUrls.push(url);
        h.openedNames.push(name);
        h.openedFeatures.push(features);
        const stubborn = { closed: false, close: (): void => undefined };
        popups.push(stubborn);
        return stubborn;
      },
    });

    const first = h.controller.reauthenticate();
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf(0)) });
    await first;
    expect(popups[0]?.closed).toBe(false); // the window is still on screen

    const second = h.controller.reauthenticate();
    await vi.advanceTimersByTimeAsync(600);
    expect(h.openedUrls).toHaveLength(1); // no window opens over the live one

    popups[0]!.closed = true; // the user closes it, or it closes at last
    await vi.advanceTimersByTimeAsync(300);
    expect(h.openedUrls).toHaveLength(2); // only now may flight two open
    h.channels[1]?.onmessage?.({ data: resultMessage(h.stateOf(1)) });
    await expect(second).resolves.toBeUndefined();
  });

  it('frees the guard after the grace period when a popup never closes', async () => {
    vi.useFakeTimers();
    const h: Harness = harness({
      closeGraceMs: 900,
      openWindow: (url, name, features) => {
        h.openedUrls.push(url);
        h.openedNames.push(name);
        h.openedFeatures.push(features);
        return { closed: false, close: (): void => undefined };
      },
    });

    const first = h.controller.reauthenticate();
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf(0)) });
    await first;

    // A popup that never closes must not kill re-auth for the page lifetime.
    const second = h.controller.reauthenticate();
    await vi.advanceTimersByTimeAsync(900);
    expect(h.openedUrls).toHaveLength(2);
    h.channels[1]?.onmessage?.({ data: resultMessage(h.stateOf(1)) });
    await expect(second).resolves.toBeUndefined();
  });

  it('names each popup after its own flight state', async () => {
    const h = harness();
    const first = h.controller.reauthenticate();
    // A fixed name makes `window.open` re-navigate the popup that is already
    // open. The name must carry the state of this flight.
    expect(h.openedNames[0]).toBe(`elitea-auth-popup-${h.stateOf(0)}`);
    h.channels[0]?.onmessage?.({ data: resultMessage(h.stateOf(0)) });
    await first;

    const second = h.controller.reauthenticate();
    h.channels[1]?.onmessage?.({ data: resultMessage(h.stateOf(1)) });
    await second;
    expect(h.openedNames[1]).not.toBe(h.openedNames[0]);
  });

  it('gives two tabs two different window names', () => {
    // Two tabs hold two controllers, so no single-flight guard can join them.
    // A shared window name lets tab B re-navigate the popup of tab A. Only a
    // per-flight name stops that.
    const tabA = harness();
    void tabA.controller.reauthenticate();
    const nameA = tabA.openedNames[0];

    // A second TAB owns a separate sessionStorage, so it cannot read the
    // marker of tab A and cannot adopt its flight.
    window.sessionStorage.clear();
    const tabB = harness();
    void tabB.controller.reauthenticate();
    expect(tabB.openedNames[0]).not.toBe(nameA);
  });
});

/**
 * The MEASURED cause of issue #364, from an instrumented WebKit run of J3.
 *
 * J3 drives `page.goto('/app/agents/all')`, which is a full document load.
 * The trace shows two documents (two `performance.timeOrigin` values), each
 * building its own controller, each starting its own flight 0.8 s apart, with
 * the popup of the first still open. No flight ended early — no settle ran
 * between the two. The guard of the controller is closure state, so it dies
 * with the document and cannot span a page load.
 *
 * sessionStorage is per tab and survives that load, so the marker crosses it.
 */
describe('issue 364 — a page load must not start a second flight', () => {
  it('adopts the flight a previous document of the same tab left running', async () => {
    const first = harness();
    void first.controller.reauthenticate();
    const liveState = first.stateOf(0);
    expect(window.sessionStorage.getItem('el.auth.state')).toBe(liveState);

    // The page loads. A NEW controller replaces the old one and its guard.
    const second = harness();
    const adopted = second.controller.reauthenticate();
    expect(second.openedUrls).toHaveLength(0); // no second popup, no second login

    // The popup of the FIRST flight reports its result to the tab, which now
    // holds the new document.
    second.channels[0]?.onmessage?.({ data: resultMessage(liveState) });
    await expect(adopted).resolves.toBeUndefined();
    expect(window.sessionStorage.getItem('el.auth.state')).toBeNull();
  });

  it('scopes the adopted listener to the state of the running flight', () => {
    const first = harness();
    void first.controller.reauthenticate();
    const second = harness();
    void second.controller.reauthenticate();
    expect(second.channels[0]?.name).toBe(`elitea-auth-${first.stateOf(0)}`);
  });

  it('reads a result that landed before the new document attached', async () => {
    const first = harness();
    void first.controller.reauthenticate();
    const liveState = first.stateOf(0);
    // The popup answered while the page was still loading, so only the
    // localStorage fallback holds the result.
    window.localStorage.setItem(
      `el.auth.result.${liveState}`,
      JSON.stringify(resultMessage(liveState)),
    );

    const second = harness();
    await expect(second.controller.reauthenticate()).resolves.toBeUndefined();
    expect(second.openedUrls).toHaveLength(0);
  });

  it('starts its own flight when the marker is older than the TTL', () => {
    const first = harness();
    void first.controller.reauthenticate();

    // The popup was abandoned. A stale marker must not block re-auth.
    const second = harness({ now: () => Date.now() + 120_000 });
    void second.controller.reauthenticate();
    expect(second.openedUrls).toHaveLength(1);
    expect(second.stateOf(0)).not.toBe(first.stateOf(0));
  });

  it('rejects an adopted flight once the marker deadline passes', async () => {
    vi.useFakeTimers();
    const first = harness();
    void first.controller.reauthenticate();

    const second = harness({ flightTtlMs: 900 });
    const adopted = second.controller.reauthenticate();
    const expectation = expect(adopted).rejects.toMatchObject({ reason: 'popup_closed' });
    await vi.advanceTimersByTimeAsync(1200);
    await expectation;
    expect(second.controller.pending).toBe(false);
  });
});

/**
 * Issue #482 — a document that is logging out must start no re-auth flight.
 *
 * The logout endpoint clears the session cookie on the first hop of a redirect
 * chain that the app document lives through, so a request the page still holds
 * open answers 401 and reaches the controller. Before this rule the flight
 * wrote `el.auth.state` and `el.auth.flight.started` back into the namespace
 * that `performLogout()` had just swept, and it showed a sign-in popup to the
 * user who asked to sign out.
 *
 * `vi.resetModules()` gives each case its own copy of the logout module, whose
 * flag is document-scoped module state. The dynamic imports below are what
 * bind the controller to THAT copy, so this exercises the real wiring and not
 * an injected stand-in.
 */
describe('re-auth is refused while the document logs out (#482)', () => {
  async function freshModules(): Promise<{
    performLogout: typeof import('./logout').performLogout;
    createController: typeof import('./popup').createAuthPopupController;
  }> {
    vi.resetModules();
    const logout = await import('./logout');
    const popup = await import('./popup');
    return { performLogout: logout.performLogout, createController: popup.createAuthPopupController };
  }

  it('opens no window, writes no flight key, and rejects with `logging_out`', async () => {
    const { performLogout, createController } = await freshModules();
    const opened: string[] = [];
    const controller = createController({
      openWindow: (url) => {
        opened.push(url);
        return { closed: false, close(): void {} };
      },
      createChannel: () => null,
    });

    performLogout({ redirect: vi.fn(), origin: 'http://app.example' });

    await expect(controller.reauthenticate()).rejects.toMatchObject({ reason: 'logging_out' });
    expect(opened).toEqual([]);
    expect(window.sessionStorage.getItem('el.auth.state')).toBeNull();
    expect(window.sessionStorage.getItem('el.auth.flight.started')).toBeNull();
    expect(controller.pending).toBe(false);
  });

  it('refuses a flight that was already running when the logout started', async () => {
    const { performLogout, createController } = await freshModules();
    const controller = createController({
      openWindow: () => ({ closed: false, close(): void {} }),
      createChannel: () => null,
    });

    const first = controller.reauthenticate();
    // The flight is live and its keys are in the namespace.
    expect(window.sessionStorage.getItem('el.auth.state')).not.toBeNull();

    performLogout({ redirect: vi.fn(), origin: 'http://app.example' });

    // The sweep took the keys...
    expect(window.sessionStorage.getItem('el.auth.state')).toBeNull();
    // ...and the next 401 does not put them back by joining the live flight.
    await expect(controller.reauthenticate()).rejects.toMatchObject({ reason: 'logging_out' });
    expect(window.sessionStorage.getItem('el.auth.state')).toBeNull();
    void first.catch(() => undefined);
  });

  it('the rule is off until performLogout() runs', async () => {
    const { createController } = await freshModules();
    const controller = createController({
      openWindow: () => ({ closed: false, close(): void {} }),
      createChannel: () => null,
    });

    void controller.reauthenticate();
    expect(window.sessionStorage.getItem('el.auth.state')).not.toBeNull();
  });
});
