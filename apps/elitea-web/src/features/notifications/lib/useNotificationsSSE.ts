/**
 * `useNotificationsSSE` — the notifications half of the socket.io → SSE
 * migration (issue #92). Replaces the `notifications_notify` socket.io
 * subscription `widgets/sidebar/ui/NotificationButton.tsx` used to carry.
 *
 * Server contract (`services/elitea-main/internal/api/v2/notifications/
 * events.go`):
 *   GET {vite_server_url}/notifications/events/prompt_lib/{projectId}
 *   - `notifications_ready` — the stream's opening cursor handshake, and
 *     also what the server sends INSTEAD of a notification whose metadata
 *     is too large for the stream contract ("the durable list remains
 *     authoritative for oversized metadata"). Both cases mean the same
 *     thing to this client: the durable list is ahead of the cache, so
 *     invalidate and let TanStack Query refetch it.
 *   - `notifications_notify` — one new notification. Optimistic signal
 *     only: it flips the caller's unread indicator on immediately, and the
 *     next authoritative list response is still free to flip it back off
 *     (see NotificationButton.tsx's own note on the one-way-ratchet bug).
 *   - `: heartbeat` comments every 15s — invisible to `EventSource`
 *     consumers by design, no handler needed.
 *
 * Auth is the session cookie (`withCredentials`, see `shared/api/sse`), and
 * the Go route's permission check is `models.notifications.notifications.
 * list` resolved against the SAME project id the list query uses.
 *
 * RECONNECT (the defect this hook used to carry). A failed stream was
 * terminal. Nothing reopened it. `onError` only wrote a `console.warn`, and
 * `useEventSource` never retries.
 *
 * The old comment claimed that the badge degrades to pre-#92 polling. No
 * polling exists. `refetchInterval` appears nowhere in the app. The unread
 * dot therefore stopped updating until the window regained focus. A remount
 * of the widget also restored it.
 *
 * The route can reject a stream with an HTTP status. It sends 429 with
 * `Retry-After: 2` when the per-principal cap of 4 streams is full. It also
 * sends 403 and 503. Per WHATWG, an HTTP status fails an `EventSource` for
 * good. A fifth tab therefore lost live notifications for the life of the
 * mount.
 *
 * The hook now reuses the shared policy in `shared/api/sse/resume.ts`. The
 * execution stream (issue #329) already defines it. The policy gives four
 * attempts at 1/2/4/8 s.
 *
 * The reopen resumes through the `cursor` query parameter. A reopened
 * `EventSource` cannot send `Last-Event-ID`. The route reads a missing cursor
 * as "start from the high-water mark". A naive reopen would therefore skip
 * every notification that arrived while the stream was down.
 *
 * Two bounds matter. A reopen happens only when the source is CLOSED. The
 * same `error` event also fires on a mid-stream drop. The browser already
 * reconnects in that case. A reopen then runs two streams for one principal,
 * and it reaches the admission cap sooner.
 *
 * An `EventSource` error shows no status and no headers. The client cannot
 * tell 403 from 429 or 503. A retry is useless for 403 and useful for the
 * other two. The bounded ladder is the whole available defence.
 *
 * Graceful degradation (§3.6): a missing project id gives a `null` url, and
 * `useEventSource` does nothing. A runtime config that failed to resolve does
 * the same. The hook reports `streamDead` when the ladder is spent. The
 * caller decides what to do with the flag. `NotificationButton` turns the
 * badge query into a slow poll. Without a live stream and without that flag,
 * the badge updates only on refocus or remount.
 *
 * CONNECTION INDICATOR. This is the app shell's always-on live channel, so
 * it reports its health into `shared/api/sse`'s real-time status store
 * (`useReportRealtimeChannel`), which the sidebar's connection dot reads:
 * `connecting` until the first `open`, `open` while the stream is up,
 * `reconnecting` during a browser-driven reconnect or a scheduled reopen,
 * and `offline` once the ladder is spent. The key is per hook instance, and
 * the report is withdrawn on unmount or when there is no url to open.
 */
import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { streamReconnectDelayMs, useEventSource, useReportRealtimeChannel, withResumeCursor, type RealtimeChannelState } from '@/shared/api/sse';
import { getConfig } from '@/shared/config';

import { NOTIFICATIONS_QUERY_ROOT } from '../api/useNotifications';

/** WHATWG `EventSource.CLOSED`. This constant is a literal number, because jsdom ships no `EventSource` constructor to read it from. */
const EVENT_SOURCE_CLOSED = 2;

export interface NotificationsStreamState {
  /** `true` once every reconnect attempt is spent. The caller owns the fallback. */
  readonly streamDead: boolean;
}

export function useNotificationsSSE(projectId: string | undefined, onNotify: () => void): NotificationsStreamState {
  const queryClient = useQueryClient();
  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : null;

  const baseUrl = useMemo(
    () => (projectId && serverUrl ? `${serverUrl}/notifications/events/prompt_lib/${projectId}` : null),
    [projectId, serverUrl],
  );

  const [streamUrl, setStreamUrl] = useState<string | null>(baseUrl);
  const [streamDead, setStreamDead] = useState(false);
  const [channelState, setChannelState] = useState<RealtimeChannelState>('connecting');
  const channelKey = `notifications:${useId()}`;
  const attemptRef = useRef(0);
  const cursorRef = useRef<string | null>(null);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const clearRetry = useCallback(() => {
    if (retryTimerRef.current === undefined) return;
    clearTimeout(retryTimerRef.current);
    retryTimerRef.current = undefined;
  }, []);

  // A new base url is a different subscription: drop any pending reopen, and
  // start the attempt budget and the cursor again.
  useEffect(() => {
    clearRetry();
    attemptRef.current = 0;
    cursorRef.current = null;
    setStreamDead(false);
    setChannelState('connecting');
    setStreamUrl(baseUrl);
  }, [baseUrl, clearRetry]);

  useEffect(() => clearRetry, [clearRetry]);

  // No url means no subscription, and so nothing to claim about it.
  useReportRealtimeChannel(channelKey, baseUrl === null ? null : channelState);

  // A spent ladder is not forever. The network coming back (`online`) or the
  // user returning to the tab (`visibilitychange` to visible) starts a fresh
  // budget, resuming from the last cursor. Without this a transient outage —
  // a laptop asleep, a burst of 429s from the per-principal cap — left the
  // stream dead and the connection dot red until a full reload.
  useEffect(() => {
    if (!streamDead || baseUrl === null) return undefined;
    const revive = (): void => {
      if (document.visibilityState === 'hidden') return;
      clearRetry();
      attemptRef.current = 0;
      setStreamDead(false);
      setChannelState('reconnecting');
      // Same null-then-url step as `scheduleReopen`: the dead stream's url can
      // be byte-identical to the resumed one, and `useEventSource` reopens
      // only on a url CHANGE.
      setStreamUrl(null);
      retryTimerRef.current = setTimeout(() => {
        retryTimerRef.current = undefined;
        setStreamUrl(withResumeCursor(baseUrl, cursorRef.current));
      }, 0);
    };
    window.addEventListener('online', revive);
    document.addEventListener('visibilitychange', revive);
    return () => {
      window.removeEventListener('online', revive);
      document.removeEventListener('visibilitychange', revive);
    };
  }, [streamDead, baseUrl, clearRetry]);

  // `id:` is written before every frame the route emits. It is the cursor a
  // resume must send back, so record it from every frame.
  const rememberCursor = useCallback((event: MessageEvent) => {
    if (event.lastEventId) cursorRef.current = event.lastEventId;
  }, []);

  const scheduleReopen = useCallback(() => {
    const attempt = attemptRef.current + 1;
    const delay = streamReconnectDelayMs(attempt);
    if (delay === undefined || baseUrl === null) {
      setStreamDead(true);
      setChannelState('offline');
      // eslint-disable-next-line no-console -- same contract as shared/api/socket/client.ts: warn, never throw or silently drop
      console.warn('notifications SSE stream failed and every reconnect attempt is spent; the badge falls back to polling');
      return;
    }
    attemptRef.current = attempt;
    setChannelState('reconnecting');
    clearRetry();
    // Clear the url first. The resumed url can be byte-identical, and
    // `useEventSource` reopens only when the url CHANGES — the same technique
    // `features/chat-messages/model/useChatStreamTransport.ts` uses.
    setStreamUrl(null);
    retryTimerRef.current = setTimeout(() => {
      retryTimerRef.current = undefined;
      setStreamUrl(withResumeCursor(baseUrl, cursorRef.current));
    }, delay);
  }, [baseUrl, clearRetry]);

  useEventSource(
    streamUrl,
    {
      notifications_notify: (event) => {
        rememberCursor(event);
        onNotify();
      },
      notifications_ready: (event) => {
        rememberCursor(event);
        void queryClient.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT });
      },
    },
    {
      // A stream that really opened proves the connection works, so the next
      // failure starts its budget from the top.
      onOpen: () => {
        attemptRef.current = 0;
        setStreamDead(false);
        setChannelState('open');
      },
      onError: (_event, readyState) => {
        // CONNECTING means the browser is reconnecting by itself. Leave the
        // stream alone; only the indicator learns about it.
        if (readyState !== EVENT_SOURCE_CLOSED) {
          setChannelState('reconnecting');
          return;
        }
        scheduleReopen();
      },
    },
  );

  return { streamDead };
}
