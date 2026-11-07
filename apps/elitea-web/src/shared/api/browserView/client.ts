/**
 * The cloud browser view stream (client contract 1.40, `viewBrowserSession`):
 * one WebSocket to elitea-main, which relays it to the session's supervisor
 * over gRPC (ADR-0049 decision 6). Shared by the web pane and the desktop's
 * streamed pane (#1592); React-free, so either can drive it.
 *
 * WHAT THIS OWNS. The pairing of each `frame` header with the ONE binary
 * message after it, ack pacing (the server sends the next frame only after
 * the ack of the one shown, always the latest), the bounds of an up message,
 * and turning a failed open into a reason a person can act on.
 *
 * A browser cannot read the HTTP status of a refused WebSocket upgrade: the
 * socket just closes with 1006. Main answers every refusal with a status
 * BEFORE the upgrade (404 not yours or no browser, 410 ended, 429 busy, 502 or
 * 503 try again), so after a socket that closed before `hello`, `diagnose`
 * asks the same route over plain HTTP and the status is the reason. A route
 * that would have admitted the view answers that request 426 (not an
 * upgrade): the failure was transient.
 *
 * AUTHENTICATION. In a browser the session cookie rides on the upgrade. The
 * desktop app authenticates with a bearer token, which the WebSocket
 * constructor cannot send; there the host supplies the socket
 * (`NativeTransport.createWebSocket`), and without one the view is not
 * available (`browserViewAvailable`).
 */
import { getConfig } from '@/shared/config';
import { getViewBrowserSessionUrl } from '@/shared/api/generated/chat/chat';
import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { getNativeTransport } from '@/shared/api/nativeTransport';

import type { BrowserViewSocketFactory } from './socket';
import {
  CLOSE_ACCESS_REVOKED,
  CLOSE_TRY_AGAIN_LATER,
  MAX_KEY_FIELD_BYTES,
  MAX_NOTE_BYTES,
  clipUtf8,
  parseDown,
  type BrowserViewCode,
  type BrowserViewFrameHeader,
  type BrowserViewHello,
  type BrowserViewUp,
} from './protocol';


/** Why a view is not (or no longer) open. */
export type { BrowserViewSocket, BrowserViewSocketFactory } from './socket';

export type BrowserViewFailure =
  /** No browser for this run, or one this person may not watch (the two are not told apart). */
  | 'not_found'
  /** The session ended, or outlived its wall time and linger. */
  | 'ended'
  /** Too many open views, or the browser service is at capacity. */
  | 'busy'
  /** The browser service or the access check did not answer; try again. */
  | 'unavailable'
  /** The person may no longer watch (4403). */
  | 'access_revoked'
  /** Signed out. */
  | 'signed_out'
  /** The page's origin is not the deployment's. */
  | 'forbidden'
  /** The connection dropped after it opened. */
  | 'disconnected';

export interface BrowserViewHandlers {
  readonly onHello?: (hello: BrowserViewHello) => void;
  /** One frame: draw it, then call `ack(header.seq)`. */
  readonly onFrame?: (header: BrowserViewFrameHeader, jpeg: Blob) => void;
  readonly onNotice?: (code: string) => void;
  readonly onRefused?: (code: string) => void;
  /** The session ended (`session_destroyed`, `idle`, `wall`, `linger_elapsed`, `agent_failed`). */
  readonly onEnd?: (code: string) => void;
  /** The view closed without an `end`. `retryable`: a reconnect may succeed. */
  readonly onFailure?: (failure: BrowserViewFailure, retryable: boolean) => void;
}

export interface BrowserViewConnection {
  ack(seq: number): void;
  pointer(kind: 'move' | 'down' | 'up', x: number, y: number, button: number, clickCount: number, modifiers: number): void;
  wheel(x: number, y: number, deltaX: number, deltaY: number, modifiers: number): void;
  key(kind: 'down' | 'up' | 'char', key: string, code: string, text: string, modifiers: number): void;
  takeOver(): void;
  handBack(note: string): void;
  close(): void;
}

const OPEN = 1;

/** Whether this build can open a view: a browser, or a desktop host that supplies the socket. */
export function browserViewAvailable(): boolean {
  const native = getNativeTransport();
  if (native !== undefined) return native.createWebSocket !== undefined;
  return typeof WebSocket === 'function';
}

/** The `ws(s)://` URL of the view, against the configured API base. */
export function browserViewUrl(sessionId: string, serverUrl: string, pageHref: string): string {
  const native = getNativeTransport();
  const base = new URL(`${serverUrl.replace(/\/+$/, '')}/`, native?.origin ?? pageHref);
  const url = new URL(getViewBrowserSessionUrl(encodeURIComponent(sessionId)).replace(/^\//, ''), base);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.toString();
}

function failureOfStatus(status: number): { failure: BrowserViewFailure; retryable: boolean } {
  switch (status) {
    case 401:
      return { failure: 'signed_out', retryable: false };
    case 403:
      return { failure: 'forbidden', retryable: false };
    case 404:
      return { failure: 'not_found', retryable: false };
    case 410:
      return { failure: 'ended', retryable: false };
    case 429:
      return { failure: 'busy', retryable: true };
    default:
      return { failure: 'unavailable', retryable: true };
  }
}

/** Why the view route refuses `sessionId` now, from its HTTP status (see the module comment). */
async function diagnose(sessionId: string): Promise<{ failure: BrowserViewFailure; retryable: boolean }> {
  // A desktop host reports its own socket's refusal, and a 426 there reads as "update the app".
  if (getNativeTransport() !== undefined) return { failure: 'unavailable', retryable: true };
  try {
    // `background`: a 401 here means the view is not available, not "sign in again" mid-chat.
    await eliteaFetch<unknown>(getViewBrowserSessionUrl(encodeURIComponent(sessionId)), { method: 'GET' }, { background: true });
    return { failure: 'unavailable', retryable: true };
  } catch (error) {
    const failure = error instanceof EliteaApiError ? error.failure : undefined;
    if (failure !== undefined && (failure.kind === 'http' || failure.kind === 'auth')) return failureOfStatus(failure.status);
    return { failure: 'unavailable', retryable: true };
  }
}

export interface OpenBrowserViewOptions {
  /** Defaults to the desktop host's socket, else `new WebSocket`. */
  readonly socketFactory?: BrowserViewSocketFactory;
  /** Defaults to `diagnose`. */
  readonly diagnose?: typeof diagnose;
  /** Defaults to the configured `vite_server_url` and the page's location. */
  readonly url?: string;
}

function defaultFactory(): BrowserViewSocketFactory {
  const native = getNativeTransport()?.createWebSocket;
  return native ?? ((url) => new WebSocket(url));
}

function defaultUrl(sessionId: string): string {
  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : '/api/v2';
  return browserViewUrl(sessionId, serverUrl, globalThis.location.href);
}

/** Open the view of `sessionId`. Nothing is sent until the server says `hello`. */
export function openBrowserView(sessionId: string, handlers: BrowserViewHandlers, options: OpenBrowserViewOptions = {}): BrowserViewConnection {
  const socket = (options.socketFactory ?? defaultFactory())(options.url ?? defaultUrl(sessionId));
  socket.binaryType = 'blob';
  let helloSeen = false;
  let ended = false;
  let closedByUs = false;
  let pending: BrowserViewFrameHeader | undefined;

  const send = (message: BrowserViewUp): void => {
    if (!helloSeen || ended || socket.readyState !== OPEN) return;
    socket.send(JSON.stringify(message));
  };

  socket.onmessage = (event: MessageEvent) => {
    const data: unknown = event.data;
    if (typeof data !== 'string') {
      // The JPEG of the header before it; a binary message without one is dropped.
      const header = pending;
      pending = undefined;
      if (header !== undefined) handlers.onFrame?.(header, data instanceof Blob ? data : new Blob([data as ArrayBuffer], { type: 'image/jpeg' }));
      return;
    }
    pending = undefined;
    const message = parseDown(data);
    if (message === undefined) return;
    if (message.type === 'hello') {
      helloSeen = true;
      handlers.onHello?.(message);
    } else if (message.type === 'frame') {
      pending = message;
    } else {
      dispatchCode(message);
    }
  };

  const dispatchCode = (message: BrowserViewCode): void => {
    if (message.type === 'notice') handlers.onNotice?.(message.code);
    else if (message.type === 'refused') handlers.onRefused?.(message.code);
    else {
      ended = true;
      handlers.onEnd?.(message.code);
    }
  };

  socket.onclose = (event: CloseEvent) => {
    if (closedByUs || ended) return;
    if (event.code === CLOSE_ACCESS_REVOKED) {
      handlers.onFailure?.('access_revoked', false);
      return;
    }
    if (helloSeen) {
      handlers.onFailure?.(event.code === CLOSE_TRY_AGAIN_LATER ? 'unavailable' : 'disconnected', true);
      return;
    }
    // Refused before the upgrade: ask the route why.
    void (options.diagnose ?? diagnose)(sessionId).then(({ failure, retryable }) => {
      if (!closedByUs) handlers.onFailure?.(failure, retryable);
    });
  };
  socket.onerror = () => undefined;

  const key = (value: string): string => clipUtf8(value, MAX_KEY_FIELD_BYTES);
  return {
    ack: (seq) => send({ type: 'ack', seq }),
    pointer: (kind, x, y, button, clickCount, modifiers) => send({ type: 'pointer', kind, x, y, button, click_count: clickCount, modifiers }),
    wheel: (x, y, deltaX, deltaY, modifiers) => send({ type: 'wheel', x, y, delta_x: deltaX, delta_y: deltaY, modifiers }),
    key: (kind, keyName, code, text, modifiers) => send({ type: 'key', kind, key: key(keyName), code: key(code), text: key(text), modifiers }),
    takeOver: () => send({ type: 'take_over' }),
    handBack: (note) => send({ type: 'hand_back', note: clipUtf8(note, MAX_NOTE_BYTES) }),
    close: () => {
      closedByUs = true;
      socket.close(1000);
    },
  };
}
