/**
 * Popup re-auth controller (spec §5.4 behaviours 3 + 4).
 *
 * Ported from apps/elitea-ui/src/[fsd]/features/auth/lib/helpers/
 * authPopup.helpers.js with two mandated fixes:
 *  - state is `crypto.randomUUID()` — the old flow used
 *    `Math.random().toString(36)+Date.now().toString(36)`
 *    (authPopup.helpers.js:42), not cryptographically random, and it is the
 *    value the callback is trusted against.
 *  - a controller INSTANCE (factory-created, injected) instead of the old
 *    module-scope promise + import-time listener side effects.
 *
 * Result delivery (all three old channels kept): opener postMessage,
 * BroadcastChannel `elitea-auth-<state>`, and the localStorage fallback
 * (storage event + poll) — read through the namespaced storage wrapper.
 */
import { createStorage } from '../../lib/storage';

import { createBroadcastChannel } from './channel';
import type { AuthChannelLike } from './channel';
import { isLoggingOut } from './logout';
import {
  AUTH_CALLBACK_PATH,
  AUTH_CHANNEL_PREFIX,
  AUTH_FLIGHT_STARTED_KEY,
  AUTH_STATE_PARAM,
  AUTH_STATE_STORAGE_KEY,
  SSO_LOGIN_PATH,
  TARGET_TO_PARAM,
  authResultStorageKey,
  authWindowName,
  isAuthResultMessage,
} from './constants';

/* Popup sizing — parity with authPopup.helpers.js:14-19. */
const MIN_WIDTH = 500;
const MIN_HEIGHT = 600;
const MAX_WIDTH = 800;
const MAX_HEIGHT = 900;
const WIDTH_RATIO = 0.4;
const HEIGHT_RATIO = 0.7;
const DEFAULT_POLL_MS = 300; // authPopup.helpers.js:96

/**
 * How long `popup.closed` must stay true before the controller believes it.
 *
 * DEFENCE, NOT THE MEASURED CAUSE of issue #364 — `AUTH_FLIGHT_STARTED_KEY`
 * carries what the measurement found. Issue #364 proposed that WebKit reports
 * `closed` as true while a popup crosses origin, and that one such reading
 * ended the flight early. An instrumented WebKit run of J3 did not observe
 * that: no flight ended early, and a 250-sample probe of `closed` across an
 * origin crossing returned no true reading. The debounce stays because one
 * reading is weak evidence in any engine, and because its cost is bounded —
 * it delays only the rejection of a flight whose popup the user closed.
 *
 * A closed window cannot navigate, so a later `closed === false` reading
 * proves the earlier one was false. The poll counts consecutive true readings
 * and any false reading resets the count.
 */
const DEFAULT_CLOSE_CONFIRM_MS = 1500;

/**
 * How long a persisted flight marker stays adoptable (issue #364).
 *
 * A new document adopts the flight of the document it replaced, so one open
 * popup keeps one flight across a page load. The bound matters in both
 * directions. Too short, and a user who takes time over the login form gets a
 * second popup. Too long, and a popup that died silently makes every later
 * request wait. Two minutes covers an ordinary login, and the recovery after
 * it is a new popup, not a failure.
 */
const DEFAULT_FLIGHT_TTL_MS = 120_000;

/**
 * Upper bound on the wait for the popup of the previous flight to go away.
 *
 * A popup that never closes must not kill re-auth for the whole page
 * lifetime. `routes/auth-callback.tsx` closes the popup only when
 * `window.opener` is present, so a popup that lost its opener stays on
 * screen. After this grace the next flight opens its own window, which its
 * own state-scoped name keeps separate from the stale one.
 */
const DEFAULT_CLOSE_GRACE_MS = 5000;

export type AuthPopupFailureReason =
  | 'popup_blocked'
  | 'popup_closed'
  | 'auth_failed'
  /** The document is logging out; see `reauthenticate()` and issue #482. */
  | 'logging_out';

export class AuthPopupError extends Error {
  readonly reason: AuthPopupFailureReason;
  constructor(reason: AuthPopupFailureReason) {
    super(`auth popup: ${reason}`);
    this.name = 'AuthPopupError';
    this.reason = reason;
  }
}

export interface PopupWindowLike {
  readonly closed: boolean;
  close(): void;
}

export interface AuthPopupOptions {
  /** Origin the callback URL is built on; default `window.location.origin`. */
  baseOrigin?: string;
  /** App base path (e.g. `/elitea_ui`); default `''`. */
  basePath?: string;
  openWindow?: (url: string, name: string, features: string) => PopupWindowLike | null;
  createChannel?: (name: string) => AuthChannelLike | null;
  pollIntervalMs?: number;
  /** Continuous time `popup.closed` must read true before it is believed. */
  closeConfirmMs?: number;
  /** Upper bound on the wait for the popup of the previous flight to go. */
  closeGraceMs?: number;
  /** How long a persisted flight marker stays adoptable. */
  flightTtlMs?: number;
  /** Clock, injected for tests. */
  now?: () => number;
  /**
   * The login entry point the popup opens, per authentication plane. The two planes are mutually exclusive.
   * `/forward-auth/auth_oidc/login` exists on one of them. A fixed OIDC path makes the popup a 404 on the other.
   *
   * PASS THE FUNCTION FORM. The plane comes from the session probe. This controller is built before the probe
   * answers. A value read at construction time is always the OIDC default. See `popup.test.ts`.
   */
  loginPath?: string | (() => string);
}

/** Reads the per-flight login path; the SSO sign-in page is the default. */
function resolveLoginPath(loginPath: AuthPopupOptions['loginPath']): string {
  return typeof loginPath === 'function' ? loginPath() : (loginPath ?? SSO_LOGIN_PATH);
}

export interface AuthPopupController {
  /**
   * Opens (or joins — single-flight) the re-auth popup. Resolves when the
   * popup reports a verified-session success for THIS flight's state;
   * rejects with `AuthPopupError` otherwise.
   */
  reauthenticate(): Promise<void>;
  readonly pending: boolean;
}

function defaultOpenWindow(url: string, name: string, features: string): PopupWindowLike | null {
  return window.open(url, name, features);
}

function popupFeatures(): string {
  const width = Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, Math.round(window.outerWidth * WIDTH_RATIO)));
  const height = Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, Math.round(window.outerHeight * HEIGHT_RATIO)));
  const left = Math.max(0, window.screenX + (window.outerWidth - width) / 2);
  const top = Math.max(0, window.screenY + (window.outerHeight - height) / 2);
  return (
    `width=${width},height=${height},left=${left},top=${top},` +
    'menubar=no,toolbar=no,location=yes,status=no,resizable=yes,scrollbars=yes'
  );
}

export function createAuthPopupController(options: AuthPopupOptions = {}): AuthPopupController {
  const openWindow = options.openWindow ?? defaultOpenWindow;
  const createChannel = options.createChannel ?? createBroadcastChannel;
  const pollIntervalMs = options.pollIntervalMs ?? DEFAULT_POLL_MS;
  const closeConfirmMs = options.closeConfirmMs ?? DEFAULT_CLOSE_CONFIRM_MS;
  const closeGraceMs = options.closeGraceMs ?? DEFAULT_CLOSE_GRACE_MS;
  const flightTtlMs = options.flightTtlMs ?? DEFAULT_FLIGHT_TTL_MS;
  const now = options.now ?? ((): number => Date.now());
  // Two readings minimum, whatever the poll interval: one reading is exactly
  // what a browser gets wrong during navigation (issue #364).
  const closedReadingsNeeded = Math.max(2, Math.ceil(closeConfirmMs / pollIntervalMs));
  const session = createStorage('session');
  const local = createStorage('local');

  let flight: Promise<void> | null = null;
  /** The popup of the most recent flight, until it is confirmed gone. */
  let livePopup: PopupWindowLike | null = null;

  /** Resolves when `popup` reports closed, or when the grace runs out. */
  function waitForPopupToGo(popup: PopupWindowLike): Promise<void> {
    return new Promise<void>((resolve) => {
      let waitedMs = 0;
      const intervalId = setInterval(() => {
        waitedMs += pollIntervalMs;
        if (!popup.closed && waitedMs < closeGraceMs) return;
        clearInterval(intervalId);
        if (livePopup === popup) livePopup = null;
        resolve();
      }, pollIntervalMs);
    });
  }

  /**
   * The state of a flight that another document of THIS tab left running, or
   * `null`. The marker is dropped once it is older than the TTL.
   */
  function adoptableState(): string | null {
    const state = session.get(AUTH_STATE_STORAGE_KEY);
    const rawStartedAt = session.get(AUTH_FLIGHT_STARTED_KEY);
    if (state === null || state === '') return null;
    // A state with no timestamp is not adoptable: `Number(null)` is 0, so the
    // deadline test alone would drop it, and by accident rather than by rule.
    const startedAt = rawStartedAt === null ? Number.NaN : Number(rawStartedAt);
    if (!Number.isFinite(startedAt) || now() - startedAt >= flightTtlMs) {
      session.remove(AUTH_STATE_STORAGE_KEY);
      session.remove(AUTH_FLIGHT_STARTED_KEY);
      return null;
    }
    return state;
  }

  /**
   * Runs one flight. `adoptedState` names a flight another document started;
   * this controller then opens NO window and only waits for the result.
   */
  function startFlight(adoptedState: string | null = null): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      // Behaviour 4: cryptographically random state, verified on delivery.
      const state = adoptedState ?? crypto.randomUUID();
      const startedAt = adoptedState === null ? now() : Number(session.get(AUTH_FLIGHT_STARTED_KEY));
      if (adoptedState === null) {
        session.set(AUTH_STATE_STORAGE_KEY, state);
        session.set(AUTH_FLIGHT_STARTED_KEY, String(startedAt));
      }

      let channel: AuthChannelLike | null = null;
      let intervalId: ReturnType<typeof setInterval> | null = null;
      let popupWindow: PopupWindowLike | null = null;
      /** Consecutive `popup.closed === true` readings; see the constant. */
      let closedReadings = 0;
      // Per-flight settle guard. It deliberately does NOT consult `flight`:
      // the executor can settle SYNCHRONOUSLY (a blocked popup does exactly
      // that), before `reauthenticate` has even assigned the slot, so any
      // slot write from in here would be clobbered by that later assignment
      // and strand the controller. Slot ownership lives in `reauthenticate`.
      let settled = false;

      const cleanup = (): void => {
        window.removeEventListener('message', onWindowMessage);
        window.removeEventListener('storage', onStorageEvent);
        if (intervalId !== null) clearInterval(intervalId);
        channel?.close();
        session.remove(AUTH_STATE_STORAGE_KEY);
        session.remove(AUTH_FLIGHT_STARTED_KEY);
        // The flight is over, so the popup has no more work. The callback
        // page closes itself, but only when `window.opener` is present
        // (routes/auth-callback.tsx). Close it here too, so a popup that lost
        // its opener cannot hold the next flight back for the whole grace.
        try {
          popupWindow?.close();
        } catch {
          // Handled (§3.6): a window we cannot close still times out below.
        }
      };

      const settle = (error: AuthPopupError | null): void => {
        if (settled) return;
        settled = true;
        cleanup();
        if (error === null) resolve();
        else reject(error);
      };

      const handleResult = (data: unknown): void => {
        // State verification: a result for any other state is ignored.
        if (!isAuthResultMessage(data) || data.state !== state) return;
        settle(data.success ? null : new AuthPopupError('auth_failed'));
      };

      const onWindowMessage = (event: MessageEvent): void => {
        if (event.origin !== window.location.origin) return;
        handleResult(event.data);
      };

      // The fallback key is state-scoped like the BroadcastChannel, so two
      // controllers (two tabs) can never consume each other's result.
      const resultKey = authResultStorageKey(state);

      const consumeStoredResult = (): void => {
        const stored = local.getJSON(resultKey, (raw) =>
          isAuthResultMessage(raw) ? raw : undefined,
        );
        if (stored === null) return;
        local.remove(resultKey);
        handleResult(stored);
      };

      const onStorageEvent = (): void => {
        consumeStoredResult();
      };

      window.addEventListener('message', onWindowMessage);
      window.addEventListener('storage', onStorageEvent);
      channel = createChannel(AUTH_CHANNEL_PREFIX + state);
      if (channel !== null) {
        channel.onmessage = (event): void => {
          handleResult(event.data);
        };
      }

      // The popup's landing page: this app's own callback route, correlated
      // by `auth_state`. It is the login endpoint's `target_to`, NOT the popup's
      // opening URL. See SSO_LOGIN_PATH's doc comment. It tells you why opening
      // it directly can never re-authenticate on a stack that does not gate the
      // SPA at the edge.
      // The login path resolves HERE, per flight: see `loginPath`.
      const callbackTarget =
        `${options.basePath ?? ''}${AUTH_CALLBACK_PATH}?${AUTH_STATE_PARAM}=${state}`;
      const popupUrl =
        `${options.baseOrigin ?? window.location.origin}${resolveLoginPath(options.loginPath)}` +
        `?${TARGET_TO_PARAM}=${encodeURIComponent(callbackTarget)}`;
      if (adoptedState === null) {
        // The window name is STATE-SCOPED, so no later `window.open` can
        // replace the page of this popup — see `authWindowName` (issue #364).
        const popup = openWindow(popupUrl, authWindowName(state), popupFeatures());
        if (popup === null) {
          settle(new AuthPopupError('popup_blocked'));
          return;
        }
        popupWindow = popup;
        livePopup = popup;
      } else {
        // Adopted: the popup belongs to the document this one replaced, so a
        // result may already sit in the fallback key. Read it before waiting.
        consumeStoredResult();
        if (settled) return;
      }

      intervalId = setInterval(() => {
        consumeStoredResult();
        if (settled) return;
        if (popupWindow === null) {
          // Adopted flight: this document holds no window handle, so it
          // cannot read `closed`. The marker deadline bounds the wait.
          if (now() - startedAt >= flightTtlMs) settle(new AuthPopupError('popup_closed'));
          return;
        }
        if (!popupWindow.closed) {
          // A closed window cannot navigate, so this reading proves every
          // earlier `closed` reading of this popup was false.
          closedReadings = 0;
          return;
        }
        closedReadings += 1;
        if (closedReadings >= closedReadingsNeeded) {
          settle(new AuthPopupError('popup_closed'));
        }
      }, pollIntervalMs);
    });
  }

  return {
    reauthenticate() {
      // A document that is logging out must NOT start a new flight (issue
      // #482). The logout endpoint clears the session cookie on the first hop
      // of a redirect chain that this document lives through, so every request
      // the page still has open then answers 401 — and each of those 401s
      // arrives here. Without this line the flight writes
      // `el.auth.state` and `el.auth.flight.started` back into the namespace
      // `performLogout()` has just swept, and the signed-out user is shown a
      // sign-in popup. Measured on WebKit: 2 failures in 60 runs of journey
      // J4, both with exactly those two keys surviving.
      //
      // It is placed BEFORE the single-flight check on purpose: a flight that
      // was already running when the user pressed "Log out" must not be
      // handed to a new caller either.
      if (isLoggingOut()) return Promise.reject(new AuthPopupError('logging_out'));
      // Controller-level single-flight (§5.4 behaviour 3 backstop; the HTTP
      // client also single-flights across its own concurrent 401s).
      if (flight !== null) return flight;
      // The guard holds until the popup GOES, not until the flight settles
      // (issue #364). A settled flight can still leave a window on screen:
      // the callback page closes itself about 300 ms after it posts the
      // result. Opening over that window would restart a login the user has
      // already answered.
      // A flight left running by another document of this tab is ADOPTED, not
      // repeated: one open popup keeps one flight across a page load.
      const begin = (): Promise<void> => startFlight(adoptableState());
      const previous = livePopup;
      const started =
        previous !== null && !previous.closed ? waitForPopupToGo(previous).then(begin) : begin();
      // Assign BEFORE attaching the release handlers: a synchronously
      // settling flight (blocked popup — the common case, since re-auth
      // fires from a background 401 with no user activation) must still end
      // up with an empty slot, and the release below only clears the slot it
      // still owns, so a later flight is never cancelled by an earlier one.
      flight = started;
      const release = (): void => {
        if (flight === started) flight = null;
      };
      void started.then(release, release);
      return started;
    },
    get pending() {
      return flight !== null;
    },
  };
}
