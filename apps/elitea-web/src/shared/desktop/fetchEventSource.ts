/**
 * An `EventSource` look-alike built on `fetch` + `ReadableStream`
 * (ADR-0029 decision 9).
 *
 * `EventSource` cannot send an `Authorization` header, and the desktop client
 * authenticates with a bearer token, so `useEventSource` (the one SSE
 * abstraction every caller goes through) asks the registered native transport
 * for this instead. It reproduces the WHATWG state machine callers already
 * depend on, deliberately including the part that bites:
 *
 *  - a non-200 answer or a non-`text/event-stream` content type fails the
 *    connection PERMANENTLY: `error`, `readyState` `CLOSED`, no retry. Callers
 *    (`useNotificationsSSE`, the chat stream) own their own reconnect on that
 *    signal, and a second automatic retry here would run two streams for one
 *    principal against the server's per-principal admission cap;
 *  - a stream that ends, or a network failure, reconnects by itself after
 *    `retry` (default 3 s, a server value floored at 1 s) with
 *    `Last-Event-ID`: `error` with `CONNECTING`;
 *  - `open` fires when the response headers arrive.
 *
 * Differences by design: a 401 is answered by one token refresh and an
 * immediate retry (the transport owns single-flight); a second 401 fails the
 * connection permanently WITHOUT signing out (the refresh just proved the
 * session alive). Only a refresh that reports `ended` signs out. Redirects are not followed (the host's
 * fetch is given `maxRedirections: 0`; see `entries/desktop/launchApp.tsx`) so
 * a bearer token cannot be forwarded to a host the user did not choose, and a
 * URL off the deployment origin never gets the token at all.
 */
import type { EventSourceLike, NativeTransport } from '@/shared/api/nativeTransport';

import { EventStreamParser } from './sseParser';

const CONNECTING = 0;
const OPEN = 1;
const CLOSED = 2;

const DEFAULT_RETRY_MS = 3000;
/** Floor for a server-sent `retry:`; `retry: 0` would otherwise reconnect in a tight loop. */
export const MIN_RETRY_MS = 1000;

type Listener = (event: MessageEvent) => void;

export interface FetchEventSourceDeps {
  transport: Pick<NativeTransport, 'accessToken' | 'refresh' | 'signOut' | 'headers' | 'fetch' | 'origin'>;
  /** Reconnect delay when the server sent no `retry:`. */
  defaultRetryMs?: number;
}

export class FetchEventSource implements EventSourceLike {
  static readonly CONNECTING = CONNECTING;
  static readonly OPEN = OPEN;
  static readonly CLOSED = CLOSED;

  readyState: number = CONNECTING;

  private readonly listeners = new Map<string, Set<Listener>>();
  private readonly controller = new AbortController();
  private lastEventId: string | null = null;
  private retryMs: number;
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined;

  private readonly url: string;
  private readonly deps: FetchEventSourceDeps;

  constructor(url: string, deps: FetchEventSourceDeps) {
    this.url = url;
    this.deps = deps;
    this.retryMs = deps.defaultRetryMs ?? DEFAULT_RETRY_MS;
    void this.run();
  }

  addEventListener(type: string, listener: Listener): void {
    let set = this.listeners.get(type);
    if (set === undefined) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(listener);
  }

  close(): void {
    this.readyState = CLOSED;
    clearTimeout(this.reconnectTimer);
    this.controller.abort();
  }

  private emit(event: Event): void {
    for (const listener of this.listeners.get(event.type) ?? []) listener(event as MessageEvent);
  }

  private fail(): void {
    if (this.readyState === CLOSED) return;
    this.readyState = CLOSED;
    this.emit(new Event('error'));
  }

  /** One connection attempt: `ended` (reconnect) or `failed` (permanent). */
  private async connect(refreshed: boolean): Promise<'ended' | 'failed'> {
    const { transport } = this.deps;
    // The bearer goes to the connected deployment and nowhere else.
    if (!this.onDeploymentOrigin()) return 'failed';
    const token = await transport.accessToken();
    const response = await (transport.fetch ?? fetch)(this.url, {
      method: 'GET',
      headers: this.requestHeaders(token),
      credentials: 'omit',
      redirect: 'error',
      signal: this.controller.signal,
    });
    if (this.readyState === CLOSED) return 'ended';

    if (response.status === 401) {
      void response.body?.cancel();
      if (refreshed) return 'failed';
      const outcome = await transport.refresh(token);
      if (outcome === 'refreshed') return this.connect(true);
      // Not renewable right now: the connection drops and retries like any other.
      return outcome === 'unavailable' ? 'ended' : this.sessionEnded();
    }
    if (!isEventStream(response) || response.body === null) {
      void response.body?.cancel();
      return 'failed';
    }
    await this.consume(response.body);
    return 'ended';
  }

  private onDeploymentOrigin(): boolean {
    const { origin } = this.deps.transport;
    return origin === undefined || new URL(this.url).origin === new URL(origin).origin;
  }

  private sessionEnded(): 'failed' {
    this.deps.transport.signOut('refresh_failed');
    return 'failed';
  }

  private requestHeaders(token: string | undefined): Headers {
    const headers = new Headers({ Accept: 'text/event-stream', 'Cache-Control': 'no-cache' });
    if (token !== undefined) headers.set('Authorization', `Bearer ${token}`);
    if (this.lastEventId !== null && this.lastEventId !== '') headers.set('Last-Event-ID', this.lastEventId);
    for (const [name, value] of Object.entries(this.deps.transport.headers ?? {})) headers.set(name, value);
    return headers;
  }

  /** Read the body to its end, delivering events as they complete. */
  private async consume(body: ReadableStream<Uint8Array>): Promise<void> {
    this.readyState = OPEN;
    this.emit(new Event('open'));
    const parser = new EventStreamParser({ lastEventId: this.lastEventId });
    const reader = body.getReader();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        this.dispatch(parser, parser.push(value));
      }
      this.dispatch(parser, parser.end());
    } finally {
      reader.cancel().catch(() => undefined);
    }
  }

  private dispatch(parser: EventStreamParser, events: ReturnType<EventStreamParser['push']>): void {
    for (const event of events) {
      if (this.readyState === CLOSED) return;
      this.emit(
        new MessageEvent(event.event, {
          data: event.data,
          lastEventId: event.id ?? '',
          origin: new URL(this.url).origin,
        }),
      );
    }
    this.lastEventId = parser.lastEventId;
    if (parser.retryMs !== null) this.retryMs = Math.max(MIN_RETRY_MS, parser.retryMs);
  }

  private async run(): Promise<void> {
    while (this.readyState !== CLOSED) {
      let outcome: 'ended' | 'failed';
      try {
        outcome = await this.connect(false);
      } catch {
        // Network error or abort: an abort means close() ran; anything else
        // is retried, as the standard does for a connection that drops.
        if (this.readyState === CLOSED) return;
        outcome = 'ended';
      }
      if (this.readyState === CLOSED) return;
      if (outcome === 'failed') {
        this.fail();
        return;
      }
      this.readyState = CONNECTING;
      this.emit(new Event('error'));
      await new Promise<void>((resolve) => {
        this.reconnectTimer = setTimeout(resolve, this.retryMs);
      });
    }
  }
}

function isEventStream(response: Response): boolean {
  return response.status === 200 && (response.headers.get('content-type') ?? '').includes('text/event-stream');
}

/** The `createEventSource` factory the desktop transport registers. */
export function fetchEventSourceFactory(deps: FetchEventSourceDeps): (url: string) => EventSourceLike {
  return (url) => new FetchEventSource(url, deps);
}
