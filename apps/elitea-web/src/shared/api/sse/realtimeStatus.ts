/**
 * Real-time channel health — what the app shell's connection indicator
 * (`widgets/sidebar/ui/SidebarConnectionDot.tsx`) reports.
 *
 * WHY THIS EXISTS. The indicator used to read the socket.io client's
 * connection state. elitea-main runs no socket.io server (it was deleted with
 * #126; see `cmd/elitea-main/main.go`'s NOTE), so every deployment ships
 * `vite_socket_server: ""`, `AppProviders` installs the noop socket client,
 * and that client's state is the constant `'disconnected'`. The dot was
 * therefore permanently red with "Disconnected" while chat streamed and
 * every API call succeeded. The live push channels the app actually depends
 * on are Server-Sent-Events streams (issue #92). The old app's dot meant
 * "the live push channel is up", so the faithful port is to watch those.
 *
 * Contract. Every always-on SSE subscription reports its own state through
 * `useReportRealtimeChannel` under a key unique to the subscribing hook
 * instance, and the report is withdrawn on unmount. The indicator
 * aggregates:
 *
 *   - the browser says it is offline           -> `offline`
 *   - no channel registered                    -> `idle` (indicator hidden:
 *     nothing to report on, so it claims nothing rather than a false red)
 *   - any channel open                         -> `connected`
 *   - any channel retrying after a failure     -> `reconnecting`
 *   - any channel still opening its first time -> `connecting`
 *   - every channel gave up                    -> `offline`
 *
 * "Any open wins" is deliberate: one healthy stream proves the session, the
 * edge and the server are all reachable, which is the question the dot
 * answers. Per-feature streams keep their own error UI.
 *
 * socket.io is NOT an input. Only a few feature-local surfaces (voice among
 * them) still speak it, each degrading on its own when it is absent; its
 * absence must not paint the whole app as offline.
 *
 * A factory plus a context, not a module-scope store (R-S2): `app/` creates
 * the one instance. With no provider mounted (unit tests, the admin bundle)
 * reports are dropped and the status is `idle`, so the indicator renders
 * nothing rather than a guess.
 */
import { createContext, useContext, useEffect, useSyncExternalStore } from 'react';

/** One subscription's state, as the hook that owns it sees it. */
export type RealtimeChannelState = 'connecting' | 'open' | 'reconnecting' | 'offline';

/** What the indicator shows. `idle` means "no channel to report on". */
export type RealtimeStatus = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'offline';

type Channels = Readonly<Record<string, RealtimeChannelState>>;

/** @public The injected instance's shape. */
export interface RealtimeStatusStore {
  /** Record `state` for `key`, or withdraw it with `null`. A no-op when nothing changes. */
  readonly report: (key: string, state: RealtimeChannelState | null) => void;
  readonly getChannels: () => Channels;
  readonly subscribe: (listener: () => void) => () => void;
}

export function createRealtimeStatusStore(): RealtimeStatusStore {
  let channels: Channels = {};
  const listeners = new Set<() => void>();
  return {
    report: (key, state) => {
      if (state === null) {
        if (!(key in channels)) return;
        const next = { ...channels };
        delete next[key];
        channels = next;
      } else {
        if (channels[key] === state) return;
        channels = { ...channels, [key]: state };
      }
      for (const listener of Array.from(listeners)) listener();
    },
    getChannels: () => channels,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** React context carrying the single injected store (`app/` owns creation). */
export const RealtimeStatusContext = createContext<RealtimeStatusStore | null>(null);

/** Pure aggregation — the whole decision table above, testable as values. */
export function aggregateRealtimeStatus(channels: Channels, browserOnline: boolean): RealtimeStatus {
  if (!browserOnline) return 'offline';
  const states = Object.values(channels);
  if (states.length === 0) return 'idle';
  if (states.includes('open')) return 'connected';
  if (states.includes('reconnecting')) return 'reconnecting';
  if (states.includes('connecting')) return 'connecting';
  return 'offline';
}

/**
 * Report one channel's state for the lifetime of the calling component.
 * `null` means "no subscription right now" and withdraws the report; so does
 * unmounting or changing `key`.
 */
export function useReportRealtimeChannel(key: string, state: RealtimeChannelState | null): void {
  const store = useContext(RealtimeStatusContext);
  useEffect(() => {
    store?.report(key, state);
  }, [store, key, state]);
  useEffect(() => () => store?.report(key, null), [store, key]);
}

const EMPTY_CHANNELS: Channels = {};
const noopSubscribe = (): (() => void) => () => undefined;
const readEmpty = (): Channels => EMPTY_CHANNELS;

function subscribeOnline(onChange: () => void): () => void {
  window.addEventListener('online', onChange);
  window.addEventListener('offline', onChange);
  return () => {
    window.removeEventListener('online', onChange);
    window.removeEventListener('offline', onChange);
  };
}

function readOnline(): boolean {
  // `onLine` is `false` only when the browser is sure there is no network;
  // anything else (including a runtime without `navigator`) counts as online.
  return typeof navigator === 'undefined' || navigator.onLine;
}

/** React hook: the aggregated status, re-rendering on channel or network changes. */
export function useRealtimeStatus(): RealtimeStatus {
  const store = useContext(RealtimeStatusContext);
  const channels = useSyncExternalStore(
    store ? store.subscribe : noopSubscribe,
    store ? store.getChannels : readEmpty,
    store ? store.getChannels : readEmpty,
  );
  const online = useSyncExternalStore(subscribeOnline, readOnline, () => true);
  return aggregateRealtimeStatus(channels, online);
}
