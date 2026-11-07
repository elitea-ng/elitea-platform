/**
 * The cloud browser view's socket on the desktop (#1592, ADR-0048 decision
 * 7): a `BrowserViewSocket` the Tauri host opens (`src-tauri/src/view_socket.rs`,
 * IPC.md "Browser view socket"). The page's `WebSocket` cannot send the
 * bearer the desktop authenticates with; the host can, and the token never
 * reaches the page.
 *
 * Wire. `browser_view_open {id, url, events}` starts the socket; its events
 * arrive on a Tauri channel, numbered (they can arrive out of order and are
 * put back in order here): `{t:"open"}`, `{t:"text", d}`, a binary message
 * as an `ArrayBuffer` (a frame's JPEG, never base64), and
 * `{t:"close", code, reason, status}` last. `browser_view_send {id,
 * messages, close}` carries the page's text messages in order (one call at
 * a time, so the host sees them in the order sent) and, last, its close.
 *
 * `refusedStatus` is the HTTP status of a refused upgrade (404, 410, 429,
 * 502, 503, 401, 426): the reason a browser's own `WebSocket` cannot read.
 */
import type { BrowserViewSocket } from '@/shared/api/browserView/socket';

const CONNECTING = 0;
const OPEN = 1;
const CLOSING = 2;
const CLOSED = 3;

/** The host refuses a message past this (view_socket.rs `MAX_UP_MESSAGE`); the route's own bound is lower. */
const MAX_SEND_BATCH = 256;
/** The host refused to open one more (`too_many_views`): read as the route's 429. */
const BUSY_STATUS = 429;

type ViewSocketInvoke = (command: string, args?: Record<string, unknown>) => Promise<unknown>;

/** What a Tauri channel callback receives: one numbered message, or the end. */
type ChannelEnvelope = { readonly index: number; readonly message: unknown } | { readonly index: number; readonly end: true };

export interface ViewSocketHost {
  invoke: ViewSocketInvoke;
  transformCallback(callback: (envelope: ChannelEnvelope) => void, once?: boolean): number;
  unregisterCallback?(id: number): void;
}

interface HostEvent {
  readonly t?: unknown;
  readonly d?: unknown;
  readonly code?: unknown;
  readonly reason?: unknown;
  readonly status?: unknown;
}

function numberOr<T>(value: unknown, fallback: T): number | T {
  return typeof value === 'number' ? value : fallback;
}

let lastId = Math.floor(Math.random() * 2 ** 31);

export class HostViewSocket implements BrowserViewSocket {
  binaryType: BinaryType = 'blob';
  readyState: number = CONNECTING;
  /** The HTTP status the route refused the upgrade with; `undefined` when it opened or was not reached. */
  refusedStatus: number | undefined = undefined;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;

  private readonly host: ViewSocketHost;
  private readonly id: number;
  private readonly callbackId: number;
  private nextIndex = 0;
  private readonly early = new Map<number, ChannelEnvelope>();
  private queue: string[] = [];
  private closeRequest: [number, string] | undefined;
  private scheduled = false;
  /** Sends go one at a time, after the open: the host reads them in order. */
  private chain: Promise<unknown>;

  constructor(url: string, host: ViewSocketHost) {
    this.host = host;
    lastId = (lastId + 1) % 2 ** 31;
    this.id = lastId;
    this.callbackId = host.transformCallback((envelope) => this.receive(envelope));
    this.chain = host.invoke('browser_view_open', { id: this.id, url, events: `__CHANNEL__:${this.callbackId}` }).catch((error: unknown) => {
      const code = (error as { code?: unknown } | null)?.code;
      this.finish(1006, '', code === 'too_many_views' ? BUSY_STATUS : undefined);
    });
  }

  send(data: string): void {
    if (this.readyState === CONNECTING) throw new DOMException('The view socket is still connecting.', 'InvalidStateError');
    if (this.readyState !== OPEN) return;
    this.queue.push(data);
    this.schedule();
  }

  close(code = 1000, reason = ''): void {
    if (this.readyState === CLOSING || this.readyState === CLOSED) return;
    this.readyState = CLOSING;
    this.closeRequest = [code, reason];
    this.schedule();
  }

  private schedule(): void {
    if (this.scheduled) return;
    this.scheduled = true;
    this.chain = this.chain.then(() => {
      this.scheduled = false;
      const messages = this.queue.splice(0, MAX_SEND_BATCH);
      if (this.queue.length > 0) this.schedule();
      const close = this.queue.length === 0 ? this.closeRequest : undefined;
      if (close !== undefined) this.closeRequest = undefined;
      if (messages.length === 0 && close === undefined) return undefined;
      // A send to a socket the host already closed: its close event is on the way.
      return this.host.invoke('browser_view_send', { id: this.id, messages, close: close ?? null }).catch(() => undefined);
    });
  }

  /** One channel message, in the host's order. */
  private receive(envelope: ChannelEnvelope): void {
    if (envelope.index !== this.nextIndex) {
      this.early.set(envelope.index, envelope);
      return;
    }
    let current: ChannelEnvelope | undefined = envelope;
    while (current !== undefined) {
      this.nextIndex += 1;
      this.handle(current);
      current = this.early.get(this.nextIndex);
      this.early.delete(this.nextIndex);
    }
  }

  private handle(envelope: ChannelEnvelope): void {
    if ('end' in envelope) {
      // The host dropped the channel without a close: the socket is gone.
      this.finish(1006, '', undefined);
      return;
    }
    const { message } = envelope;
    if (message instanceof ArrayBuffer) this.binary(message);
    else this.event(message ?? {});
  }

  private binary(message: ArrayBuffer): void {
    if (this.readyState !== OPEN) return;
    const data = this.binaryType === 'blob' ? new Blob([message], { type: 'image/jpeg' }) : message;
    this.onmessage?.(new MessageEvent('message', { data }));
  }

  private event(event: HostEvent): void {
    if (event.t === 'open') {
      if (this.readyState !== CONNECTING) return;
      this.readyState = OPEN;
      this.onopen?.(new Event('open'));
    } else if (event.t === 'text') {
      if (this.readyState === OPEN && typeof event.d === 'string') this.onmessage?.(new MessageEvent('message', { data: event.d }));
    } else if (event.t === 'close') {
      this.finish(numberOr(event.code, 1006), typeof event.reason === 'string' ? event.reason : '', numberOr(event.status, undefined));
    }
  }

  private finish(code: number, reason: string, status: number | undefined): void {
    if (this.readyState === CLOSED) return;
    this.readyState = CLOSED;
    this.refusedStatus = status;
    this.queue = [];
    this.closeRequest = undefined;
    this.host.unregisterCallback?.(this.callbackId);
    this.onclose?.(new CloseEvent('close', { code, reason, wasClean: code !== 1006 }));
  }
}

/** The host's IPC for the socket, or `undefined` outside the Tauri webview. */
export function tauriViewSocketHost(): ViewSocketHost | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<ViewSocketHost> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  return {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
    ...(internals.unregisterCallback !== undefined ? { unregisterCallback: internals.unregisterCallback.bind(internals) } : {}),
  };
}

/** The `createWebSocket` the desktop transport registers. */
export function hostViewSocketFactory(host: ViewSocketHost): (url: string) => BrowserViewSocket {
  return (url) => new HostViewSocket(url, host);
}
