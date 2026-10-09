/**
 * `useEventSource` — the shared Server-Sent-Events subscription primitive
 * (issue #92, the socket.io → native SSE migration).
 *
 * The backend is retiring the socket.io fan-out surface by surface;
 * `services/elitea-main/internal/api/v2/notifications/events.go` is
 * the first one already live in Go (`GET /api/v2/notifications/events/
 * prompt_lib/{projectID}`, `text/event-stream`). In the E2E compose stack
 * `VITE_SOCKET_SERVER=""`, so socket.io is permanently dead there — a
 * surface that has not moved to SSE simply never receives a live push.
 *
 * Deliberately NOT a client object with an injected factory (the shape
 * `shared/api/socket/client.ts` uses): `EventSource` is receive-only, has no
 * emit path, no connection-state store and no room bookkeeping, so there is
 * nothing for a wrapper instance to own. A hook that opens one connection
 * per (url, event-name-set) and closes it on unmount is the whole contract.
 *
 * Auth: `withCredentials: true`, always. `EventSource` cannot set an
 * `Authorization` header (the WHATWG constructor takes `withCredentials`
 * and nothing else), so the stream authenticates with the session cookie —
 * which is exactly what the Go route's `apimw.Auth` middleware validates.
 *
 * Graceful degradation (§3.6 — errors are values at the boundary, never a
 * throw): a falsy `url` (config missing, project id not resolved yet) and a
 * runtime with no `EventSource` constructor at all (jsdom, SSR) both make
 * this hook a no-op rather than a crash. Tests that need a real connection
 * install the double in `./testing.ts`.
 */
import { useEffect, useRef } from 'react';

import { getNativeTransport } from '../nativeTransport';

/**
 * Handlers keyed by SSE event name (the `event:` field on the wire), e.g.
 * `notifications_notify`. The reserved names `open` and `error` work too —
 * they are plain `EventSource` events and are registered the same way, so a
 * caller can observe connection failures without this hook needing an extra
 * option for it.
 *
 * Deliberately NOT exported (knip's dead-export gate): callers pass an
 * object literal, which infers structurally. Export it the day a consumer
 * genuinely needs to name the type.
 */
type EventSourceHandlers = Readonly<Record<string, (event: MessageEvent) => void>>;

/**
 * Options beyond the named-event handlers.
 *
 * `onError` matters more than it looks: per WHATWG, `EventSource`
 * reconnects only after a CLEAN stream end. An HTTP error status or a wrong
 * content type fails the connection PERMANENTLY, with no retry — and both
 * Go SSE routes answer with exactly those. `notifications/events.go`
 * returns 429 (`Retry-After: 2`) once its per-principal admission cap of 4
 * concurrent streams is saturated, 403 on authorize failure and 503 when the
 * reader is down; `executions/events.go` does the same. Without this
 * callback a dead stream is invisible to the app, so a caller cannot fall
 * back, warn, or retry. Every caller should pass one.
 */
interface EventSourceOptions {
  /**
   * Fired on a failed connection or a mid-stream drop. The stream is NOT
   * retried automatically — decide here.
   *
   * `readyState` is the source's WHATWG state at the moment of the error, and
   * it is the only thing that separates the two cases: `CLOSED` (2) means the
   * connection failed for good and a caller that wants it back must reopen it;
   * `CONNECTING` (0) means the browser is already reconnecting by itself. A
   * caller that reopens then runs two streams for one principal. It burns the
   * server's per-principal admission cap twice as fast.
   */
  readonly onError?: ((event: Event, readyState: number) => void) | undefined;
  /**
   * Fired once the connection is actually established (the WHATWG `open`
   * event — readyState transitions to OPEN). Distinct from a frame ever
   * arriving: `open` fires as soon as the HTTP response headers come back
   * successfully (200 + `text/event-stream`), before any `data:` line is
   * written. A route that answers a non-2xx status or the wrong content
   * type fails straight to `error` and never fires this at all — which is
   * exactly what makes it useful as a "was this ever a real stream"
   * signal for a caller that must not treat a LATER drop the same as a
   * connection that never opened (issue #310's
   * `useToolkitChatSocket.hooks.ts`).
   */
  readonly onOpen?: ((event: Event) => void) | undefined;
}

/**
 * Subscribe to `url` for the lifetime of the calling component.
 *
 * The connection is (re)opened when `url` changes or when the SET of event
 * names changes — never when a handler's identity changes. Callers
 * overwhelmingly pass a fresh object literal with fresh closures every
 * render (`{ notifications_notify: () => setHasUnread(true) }`); keeping the
 * handlers in a ref is what stops that from tearing down and reopening a
 * real HTTP stream on every single render.
 */
export function useEventSource(url: string | null | undefined, handlers: EventSourceHandlers, options: EventSourceOptions = {}): void {
  const handlersRef = useRef(handlers);
  const onErrorRef = useRef(options.onError);
  const onOpenRef = useRef(options.onOpen);
  useEffect(() => {
    handlersRef.current = handlers;
    onErrorRef.current = options.onError;
    onOpenRef.current = options.onOpen;
  });

  // Sorted + joined so the deps entry is order-insensitive: `{a, b}` and
  // `{b, a}` describe the same subscription and must not reconnect.
  const eventNamesKey = Object.keys(handlers).sort().join('\u0000');

  useEffect(() => {
    if (!url) return undefined;
    // jsdom (the `node` vitest project) and any non-browser runtime ship no
    // EventSource; `./testing.ts` installs a double when a test needs one.
    // A native client (desktop) authenticates with a bearer header, which the
    // browser constructor cannot send, so it supplies a fetch-based stream
    // with the same surface. No transport registered = the browser path below.
    const nativeFactory = getNativeTransport()?.createEventSource;
    if (nativeFactory === undefined && typeof EventSource === 'undefined') return undefined;

    const source = nativeFactory?.(url) ?? new EventSource(url, { withCredentials: true });
    // Split on the same separator the key was built with, and skip the
    // empty-string artefact an empty handler map would produce.
    const eventNames = eventNamesKey === '' ? [] : eventNamesKey.split('\u0000');
    for (const name of eventNames) {
      source.addEventListener(name, (event) => {
        handlersRef.current[name]?.(event);
      });
    }
    // Registered unconditionally, not from `handlers`: `open`/`error` are
    // reserved EventSource events, so routing them through the named-handler
    // map would put them in the connection's own cache key.
    source.addEventListener('open', (event) => {
      onOpenRef.current?.(event);
    });
    source.addEventListener('error', (event) => {
      onErrorRef.current?.(event, source.readyState);
    });
    return () => source.close();
  }, [url, eventNamesKey]);
}
