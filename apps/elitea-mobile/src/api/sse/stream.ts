import type { AppStateStatus } from 'react-native';

import { CLIENT_VERSION_HEADER, getClientVersion } from '@/api/clientVersion';
import { ApiError, parseRetryAfter } from '@/api/errors';
import { classifySessionFailure, sessionBearerToken, shouldReplay } from '@/api/middleware';
import { isSameOrigin } from '@/api/paths';
import { EventStreamParser, type SseEvent } from '@/api/sse/parser';
import { expoStreamTransport, type StreamResponse, type StreamTransport } from '@/api/sse/transport';
import { registerAbortable } from '@/lib/abortRegistry';
import { appActivity } from '@/lib/appActivity';
import { log } from '@/lib/log';
import { getSession } from '@/state/session';

/**
 * A resumable event stream (execution events, notifications).
 *
 * - Sends `Accept: text/event-stream`, Bearer, `X-Client-Version`, and
 *   `Last-Event-ID: <cursor>` once a cursor exists. Never `?cursor=` as well:
 *   the server answers 400 when both are present and differ.
 * - The cursor follows EVERY event id the parser sees, including events the
 *   consumer ignores and events without data.
 * - Watchdog: no bytes for `watchdogMs` (heartbeats arrive every 2–15 s)
 *   aborts the connection and reconnects.
 * - Backoff: full jitter, `baseDelayMs` × 2^n capped at `maxDelayMs`, reset
 *   once a connection was healthy: it dispatched an event, or it answered 200,
 *   delivered bytes (heartbeat comments count) and stayed open for at least
 *   `healthyAfterMs`. Its drop is then not counted as a failed reconnect. A
 *   200 that sends a heartbeat and closes at once (a misbehaving server, a
 *   captive portal) is not healthy, so it still backs off and gives up. A server `retry:` field replaces `baseDelayMs` (bounded
 *   by `maxDelayMs`, floored at `MIN_SERVER_RETRY_MS`) for this stream from
 *   then on, as the WHATWG reconnection time does. `Retry-After` overrides
 *   both on 429/503.
 * - No retry on 400/403/404 (or any other 4xx). 401/426 go through the same
 *   session classification as REST (`classifySessionFailure`).
 * - After `giveUpAfterMs` of failing reconnects (none of which was healthy) the
 *   stream stops retrying and reports `gave_up`; `reconnect()` starts again.
 * - App in background (or locked: see `appActivity`): the connection is
 *   aborted and the cursor kept; back in the foreground and unlocked it resumes. ('inactive' is ignored: iOS passes through it
 *   for system sheets.)
 * - The consumer returns 'stop' from `onEvent` on a terminal frame: the server
 *   never closes an execution stream itself, and an open stream holds one of
 *   its admission slots.
 */

export type StreamStatus =
  | 'connecting'
  | 'open'
  | 'retrying'
  | 'paused'
  | 'gave_up'
  /** Stopped for good: closed by the consumer, or a final failure (see `error`). */
  | 'closed';

export interface StreamStatusInfo {
  status: StreamStatus;
  /** Set when the stream stopped because of a failure. */
  error?: ApiError;
  /** Delay before the next attempt, when retrying. */
  retryInMs?: number;
}

interface AppStateLike {
  currentState: AppStateStatus | null | undefined;
  addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void };
}

export interface ResumableStreamOptions {
  /** Absolute URL on the workspace origin (validate server-supplied paths first). */
  url: string;
  /** Called for every dispatched event. Return 'stop' to close the stream. */
  onEvent: (event: SseEvent) => 'stop' | void;
  onStatus?: (info: StreamStatusInfo) => void;
  /** Resume cursor from an earlier stream, if any. */
  initialCursor?: string | null;
  /** Abort-registry scope (e.g. `projectScope(id)`). */
  scope?: string | null;
  transport?: StreamTransport;
  appState?: AppStateLike | null;
  watchdogMs?: number;
  baseDelayMs?: number;
  maxDelayMs?: number;
  giveUpAfterMs?: number;
  /** How long a 200 carrying only heartbeats must stay open to count as healthy. */
  healthyAfterMs?: number;
  random?: () => number;
  now?: () => number;
  /** Injectable for tests. */
  classifyFailure?: typeof classifySessionFailure;
}

export const STREAM_DEFAULTS = {
  watchdogMs: 30_000,
  baseDelayMs: 500,
  maxDelayMs: 15_000,
  giveUpAfterMs: 120_000,
  healthyAfterMs: 5_000,
} as const;

/** Full-jitter backoff delay for attempt `n` (0-based). */
export function backoffDelay(
  attempt: number,
  random: () => number = Math.random,
  baseMs: number = STREAM_DEFAULTS.baseDelayMs,
  capMs: number = STREAM_DEFAULTS.maxDelayMs,
): number {
  const ceiling = Math.min(capMs, baseMs * 2 ** Math.min(attempt, 30));
  return Math.floor(random() * ceiling);
}

/**
 * Floor for a server-supplied `retry:` base, so `retry: 0` cannot turn a
 * failing network into a reconnect loop with no delay at all.
 */
export const MIN_SERVER_RETRY_MS = 100;

const MAX_ERROR_BODY_BYTES = 4096;

/** Reads a short error body as JSON; anything longer or unparsable is undefined. */
async function readSmallJson(body: AsyncIterable<Uint8Array> | null): Promise<unknown> {
  if (!body) return undefined;
  const parts: Uint8Array[] = [];
  let size = 0;
  for await (const chunk of body) {
    size += chunk.byteLength;
    if (size > MAX_ERROR_BODY_BYTES) return undefined;
    parts.push(chunk);
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const part of parts) {
    bytes.set(part, offset);
    offset += part.byteLength;
  }
  return JSON.parse(new TextDecoder().decode(bytes));
}

type Outcome =
  | { kind: 'stopped' }
  | { kind: 'paused' }
  | { kind: 'final'; error: ApiError }
  /** `opened`: the connection was healthy (an event, or bytes for `healthyAfterMs`). */
  | { kind: 'retry'; retryAfterMs?: number; opened: boolean };

export class ResumableStream {
  private readonly opts: Required<
    Pick<ResumableStreamOptions, 'watchdogMs' | 'baseDelayMs' | 'maxDelayMs' | 'giveUpAfterMs' | 'healthyAfterMs' | 'random' | 'now'>
  > &
    ResumableStreamOptions;
  private cursor: string | null;
  private closed = false;
  private paused = false;
  /** Reconnects after a native token refresh since the last open stream (bounded). */
  private authRetries = 0;
  private gaveUp = false;
  private running = false;
  private controller: AbortController | null = null;
  private wake: (() => void) | null = null;
  private appSub: { remove(): void } | null = null;
  private status: StreamStatus | null = null;
  /** The last `retry:` value the server sent on any connection of this stream. */
  private serverRetryMs: number | null = null;

  constructor(options: ResumableStreamOptions) {
    this.opts = {
      ...options,
      watchdogMs: options.watchdogMs ?? STREAM_DEFAULTS.watchdogMs,
      baseDelayMs: options.baseDelayMs ?? STREAM_DEFAULTS.baseDelayMs,
      maxDelayMs: options.maxDelayMs ?? STREAM_DEFAULTS.maxDelayMs,
      giveUpAfterMs: options.giveUpAfterMs ?? STREAM_DEFAULTS.giveUpAfterMs,
      healthyAfterMs: options.healthyAfterMs ?? STREAM_DEFAULTS.healthyAfterMs,
      random: options.random ?? Math.random,
      now: options.now ?? Date.now,
    };
    this.cursor = options.initialCursor ?? null;
  }

  /** The resume cursor: the last event id seen. */
  get lastEventId(): string | null {
    return this.cursor;
  }

  get currentStatus(): StreamStatus | null {
    return this.status;
  }

  /** Starts the stream (idempotent). The returned promise settles when it closes. */
  start(): Promise<void> {
    if (this.running || this.closed) return Promise.resolve();
    this.running = true;
    const appState = this.opts.appState === undefined ? appActivity : this.opts.appState;
    if (appState) {
      if (appState.currentState === 'background') this.paused = true;
      this.appSub = appState.addEventListener('change', (state) => this.onAppState(state));
    }
    return this.loop().finally(() => {
      this.running = false;
      this.appSub?.remove();
      this.appSub = null;
    });
  }

  /** Closes the stream for good and aborts the connection. Idempotent. */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.controller?.abort(new Error('stream_closed'));
    this.wakeUp();
  }

  /** After `gave_up`: try again now, with a fresh give-up window. */
  reconnect(): void {
    if (this.closed) return;
    this.gaveUp = false;
    this.wakeUp();
  }

  private onAppState(state: AppStateStatus) {
    if (state === 'background') {
      if (this.paused || this.closed) return;
      this.paused = true;
      this.controller?.abort(new Error('app_backgrounded'));
      this.wakeUp();
    } else if (state === 'active') {
      if (!this.paused) return;
      this.paused = false;
      this.wakeUp();
    }
  }

  private wakeUp() {
    const wake = this.wake;
    this.wake = null;
    wake?.();
  }

  private setStatus(info: StreamStatusInfo) {
    this.status = info.status;
    try {
      this.opts.onStatus?.(info);
    } catch (error) {
      log.warn('stream status listener failed', { error });
    }
  }

  /** Sleeps up to `ms` (forever when undefined); close/pause/resume/reconnect wake it early. */
  private sleep(ms?: number): Promise<void> {
    return new Promise((resolve) => {
      let timer: ReturnType<typeof setTimeout> | null = null;
      const done = () => {
        if (timer !== null) clearTimeout(timer);
        if (this.wake === done) this.wake = null;
        resolve();
      };
      this.wake = done;
      if (ms !== undefined) timer = setTimeout(done, ms);
    });
  }

  private async loop(): Promise<void> {
    let attempt = 0;
    let failingSince: number | null = null;
    while (!this.closed) {
      if (this.paused) {
        this.setStatus({ status: 'paused' });
        await this.sleep();
        continue;
      }
      if (this.gaveUp) {
        this.setStatus({ status: 'gave_up' });
        await this.sleep();
        if (!this.gaveUp) {
          attempt = 0;
          failingSince = null;
        }
        continue;
      }

      this.setStatus({ status: 'connecting' });
      const outcome = await this.connectOnce();
      if (this.closed || outcome.kind === 'stopped') break;
      if (outcome.kind === 'final') {
        this.closed = true;
        this.setStatus({ status: 'closed', error: outcome.error });
        return;
      }
      if (outcome.kind === 'paused') continue;

      if (outcome.opened) {
        attempt = 0;
        failingSince = null;
      }
      const now = this.opts.now();
      failingSince ??= now;
      if (now - failingSince >= this.opts.giveUpAfterMs) {
        this.gaveUp = true;
        continue;
      }
      const delay =
        outcome.retryAfterMs ?? backoffDelay(attempt, this.opts.random, this.baseDelayMs(), this.opts.maxDelayMs);
      attempt += 1;
      this.setStatus({ status: 'retrying', retryInMs: delay });
      await this.sleep(delay);
    }
    this.setStatus({ status: 'closed' });
  }

  /** Backoff base: the server's `retry:` value when it sent one, bounded; else `baseDelayMs`. */
  private baseDelayMs(): number {
    if (this.serverRetryMs === null) return this.opts.baseDelayMs;
    return Math.min(this.opts.maxDelayMs, Math.max(MIN_SERVER_RETRY_MS, this.serverRetryMs));
  }

  private async connectOnce(): Promise<Outcome> {
    const { url } = this.opts;
    // Origin guard before any credential is read.
    const origin = getSession().serverOrigin;
    if (origin === null || !isSameOrigin(url, origin)) {
      return { kind: 'final', error: new ApiError('origin_mismatch', 'stream targets another origin') };
    }
    if (getSession().ending) {
      return { kind: 'final', error: new ApiError('session_invalid', 'not signed in', { reason: 'no_credential' }) };
    }
    const workspaceId = getSession().workspaceId;
    let token: string;
    try {
      // A native access token is refreshed first when it is about to expire.
      token = await sessionBearerToken();
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'network') return { kind: 'retry', opened: false };
      return {
        kind: 'final',
        error: error instanceof ApiError ? error : new ApiError('session_invalid', 'not signed in', { reason: 'no_credential' }),
      };
    }
    if (this.closed) return { kind: 'stopped' };

    const headers: Record<string, string> = {
      Accept: 'text/event-stream',
      Authorization: `Bearer ${token}`,
      [CLIENT_VERSION_HEADER]: getClientVersion(),
      'Cache-Control': 'no-cache',
    };
    if (this.cursor !== null) headers['Last-Event-ID'] = this.cursor;

    const controller = new AbortController();
    this.controller = controller;
    const unregister = registerAbortable(controller, this.opts.scope ?? null);
    let watchdogFired = false;
    let watchdog: ReturnType<typeof setTimeout> | null = null;
    const armWatchdog = () => {
      if (watchdog !== null) clearTimeout(watchdog);
      watchdog = setTimeout(() => {
        watchdogFired = true;
        controller.abort(new Error('stream_watchdog'));
      }, this.opts.watchdogMs);
    };
    let gotEvent = false;
    let gotBytes = false;
    let openedAt = 0;
    const healthy = () =>
      gotEvent || (gotBytes && this.opts.now() - openedAt >= this.opts.healthyAfterMs);

    try {
      armWatchdog();
      const transport = this.opts.transport ?? expoStreamTransport;
      const response = await transport(url, { headers, signal: controller.signal });

      if (response.url && !isSameOrigin(response.url, origin)) {
        return { kind: 'final', error: new ApiError('origin_mismatch', 'stream was redirected to another origin') };
      }
      if (response.status !== 200) {
        return await this.classifyStatus(response, token, workspaceId);
      }

      this.setStatus({ status: 'open' });
      this.authRetries = 0;
      openedAt = this.opts.now();
      const parser = new EventStreamParser({ lastEventId: this.cursor });
      if (response.body) {
        for await (const chunk of response.body) {
          armWatchdog();
          if (chunk.byteLength > 0) gotBytes = true;
          const events = parser.push(chunk);
          if (parser.retryMs !== null) this.serverRetryMs = parser.retryMs;
          for (const event of events) {
            gotEvent = true;
            this.cursor = event.id;
            let verdict: 'stop' | void = undefined;
            try {
              verdict = this.opts.onEvent(event);
            } catch (error) {
              log.warn('stream event handler failed', { error });
            }
            if (verdict === 'stop') {
              this.close();
              return { kind: 'stopped' };
            }
            if (this.closed) return { kind: 'stopped' };
          }
          // Ids of events that were not dispatched (no data) move the cursor too.
          this.cursor = parser.lastEventId;
        }
      }
      // The server ended the stream (or the body was empty): reconnect.
      return { kind: 'retry', opened: healthy() };
    } catch (error) {
      if (this.closed) return { kind: 'stopped' };
      if (this.paused) return { kind: 'paused' };
      if (!watchdogFired) log.debug('event stream connection failed', { error });
      return { kind: 'retry', opened: healthy() };
    } finally {
      if (watchdog !== null) clearTimeout(watchdog);
      unregister();
      if (this.controller === controller) this.controller = null;
    }
  }

  private async classifyStatus(response: StreamResponse, token: string, workspaceId: string | null): Promise<Outcome> {
    const { status } = response;
    if (status === 401 || status === 426) {
      const classify = this.opts.classifyFailure ?? classifySessionFailure;
      const error =
        (await classify({ status, json: () => readSmallJson(response.body) }, { token, sessionAuth: true, workspaceId })) ??
        new ApiError('http', `HTTP ${status}`, { status });
      // The native access token was renewed for this 401: reconnect with it
      // straight away (once per open, so a server that keeps refusing cannot
      // drive a refresh loop).
      if (shouldReplay(error) && this.authRetries < 1) {
        this.authRetries += 1;
        return { kind: 'retry', retryAfterMs: 0, opened: false };
      }
      if (error.kind === 'network') return { kind: 'retry', opened: false };
      return { kind: 'final', error };
    }
    if (status === 429 || status === 503) {
      return { kind: 'retry', retryAfterMs: parseRetryAfter(response.headers.get('Retry-After')), opened: false };
    }
    if (status === 408 || status >= 500) return { kind: 'retry', opened: false };
    return { kind: 'final', error: new ApiError('http', `HTTP ${status}`, { status }) };
  }
}
