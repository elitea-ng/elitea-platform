/**
 * Typed socket.io client wrapper (unit S5, spec §5.5). The only other place
 * in `shared/api/socket/**` (besides this file) that imports
 * `socket.io-client` is testing.ts's type-only reference (R-A3).
 *
 * A FACTORY, not a singleton (R-S2): `createSocketClient(cfg)` — the app
 * layer (R2, Wave 1) creates exactly one instance and injects it via
 * `SocketClientContext`. No module-scope `export const client = ...`
 * anywhere in this tree.
 *
 * Preserved from the old app (src/[fsd]/app/root.jsx:38-46):
 *   - `path` from config, `reconnectionDelayMax: 2000`.
 * Deliberately DROPPED: `extraHeaders` bearer token. Production already
 * relies on the handshake cookie; the old app's dev-only bearer
 * (root.jsx:44, `DEV && VITE_DEV_TOKEN`) has no equivalent in
 * shared/config's ConfigSchema — D10/W-001 removed `vite_dev_token`
 * entirely for leaking a static bearer into a world-readable runtime
 * config file, so there is nothing left to attach.
 */
import { createContext, useContext } from 'react';
import { io as defaultIoFactory, type Socket } from 'socket.io-client';
import { create } from 'zustand';

import {
  SOCKET_EVENTS,
  type EmitPayloadOf,
  type EmittableEventName,
  type ReceivePayloadOf,
  type ReceivableEventName,
} from './events';

/** @public Wave-1 surface: consumed by rooms.ts, testing.ts, and Wave-2 chat features. */
export type ConnectionState = 'connecting' | 'connected' | 'reconnecting' | 'disconnected' | 'error';

/** @public Wave-1 surface: the per-client connection store shape (the sidebar indicator no longer reads it — it reports SSE health via `shared/api/sse`). */
export interface ConnectionStoreState {
  readonly status: ConnectionState;
  readonly lastError: string | null;
}

/** @public Wave-1 surface: consumed by rooms.ts and testing.ts. */
export interface SocketClient {
  /** Escape hatch for call sites that need the raw socket.io-client instance (e.g. binary ASR chunk emits). Prefer emit/on. */
  readonly socket: Socket;
  /** Non-reactive read of the current connection status (e.g. for one-off checks outside React). */
  getConnectionState(): ConnectionState;
  /** React hook: subscribes the calling component to connection-state changes. */
  useConnectionState(): ConnectionState;
  /** Validates `payload` against the event's generated emit schema (warns, does not throw, on mismatch — §5.5 "never crashing or silently dropping") and forwards to socket.io. */
  emit<E extends EmittableEventName>(event: E, payload: EmitPayloadOf<E>): boolean;
  /** Wraps `handler` so every inbound payload is validated against the event's generated receive schema before delivery. */
  on<E extends ReceivableEventName>(event: E, handler: (payload: ReceivePayloadOf<E>) => void): void;
  off<E extends ReceivableEventName>(event: E, handler: (payload: ReceivePayloadOf<E>) => void): void;
  disconnect(): void;
}

/** The shape of socket.io-client's `io` export — named so tests can build a compatible stand-in without fighting its overloaded signature. */
export type SocketIoFactory = typeof defaultIoFactory;

export interface SocketClientConfig {
  /** Socket.IO server origin — F3 shared/config's `vite_socket_server`. */
  url: string;
  /** Socket.IO path — F3 shared/config's `vite_socket_path`; omitted falls back to socket.io-client's own default ('/socket.io'). */
  path?: string;
  /** Injected io() factory. Overridable so client.test.ts can simulate socket.io lifecycle events without opening a real connection — production callers never pass this. */
  ioFactory?: SocketIoFactory;
}

/** React context carrying the single injected instance (R-S2: app/ owns creation; this module only owns the shape). */
export const SocketClientContext = createContext<SocketClient | null>(null);

/** @public Wave-1 surface: consumed by rooms.ts and Wave-2 chat features. Throws with a clear message if no provider is mounted — a missing provider is a programmer error, not a recoverable state (§3.6). */
export function useSocketClient(): SocketClient {
  const client = useContext(SocketClientContext);
  if (client === null) {
    throw new Error('useSocketClient: no SocketClientContext.Provider is mounted above this component');
  }
  return client;
}

function warnPayloadMismatch(direction: 'emit' | 'receive', event: string, issues: unknown): void {
  // eslint-disable-next-line no-console -- deliberate: §5.5 payload validation is a warning, not a throw or a silent drop
  console.warn(`socket/client: ${direction}("${event}") payload failed schema validation`, issues);
}

/**
 * A no-op SocketClient for environments where VITE_SOCKET_SERVER is absent or
 * empty (e.g. E2E compose, offline dev). The connection state is permanently
 * 'disconnected'; emit/on/off are harmless no-ops. `useSocketClient()` callers
 * receive this instead of throwing, so socket-dependent UI renders in a
 * degraded-but-functional state rather than crashing.
 */
export function createNoopSocketClient(): SocketClient {
  const useConnectionStore = create<ConnectionStoreState>(() => ({ status: 'disconnected', lastError: null }));
  const noopSocket = {
    on: () => { /* noop */ },
    off: () => { /* noop */ },
    emit: () => false,
    disconnect: () => { /* noop */ },
    // Manager is needed for socket.io-client's Socket shape only
    io: { on: () => { /* noop */ }, off: () => { /* noop */ } },
  } as unknown as Socket;

  return {
    socket: noopSocket,
    getConnectionState: () => 'disconnected',
    useConnectionState: () => useConnectionStore((s) => s.status),
    emit: () => false,
    on: () => { /* noop */ },
    off: () => { /* noop */ },
    disconnect: () => { /* noop */ },
  };
}

export function createSocketClient(cfg: SocketClientConfig): SocketClient {
  if (typeof cfg.url !== 'string' || cfg.url === '') {
    throw new TypeError('socket/client: SocketClientConfig.url is required'); // programmer error, mirrors http.ts's createHttpClient
  }

  const ioFactory = cfg.ioFactory ?? defaultIoFactory;
  const socket = ioFactory(cfg.url, {
    ...(cfg.path !== undefined ? { path: cfg.path } : {}),
    reconnectionDelayMax: 2000,
  });

  const useConnectionStore = create<ConnectionStoreState>(() => ({ status: 'connecting', lastError: null }));
  const setStatus = (status: ConnectionState, lastError: string | null = null): void => {
    useConnectionStore.setState({ status, lastError });
  };

  // Connection-state machine: connecting -> connected -> reconnecting -> disconnected -> error.
  // Event names verified against the installed socket.io-client@4.8.3 typings
  // (node_modules/socket.io-client/build/esm/{socket,manager}.d.ts):
  //   Socket: connect, connect_error, disconnect (SocketReservedEvents)
  //   Manager (via socket.io): reconnect_attempt, reconnect, reconnect_error, reconnect_failed (ManagerReservedEvents)
  socket.on('connect', () => setStatus('connected'));
  socket.on('connect_error', (err) => setStatus('error', err.message));
  socket.on('disconnect', (reason) => setStatus('disconnected', reason));
  socket.io.on('reconnect_attempt', () => setStatus('reconnecting'));
  socket.io.on('reconnect', () => setStatus('connected'));
  socket.io.on('reconnect_error', (err) => setStatus('reconnecting', err.message));
  socket.io.on('reconnect_failed', () => setStatus('error', 'reconnect_failed: exhausted reconnection attempts'));

  // Map from the caller's handler to the wrapped (validating) listener actually
  // registered with socket.io, so off() can remove the exact same function.
  const wrappedListeners = new WeakMap<(payload: never) => void, (payload: unknown) => void>();

  function emit<E extends EmittableEventName>(event: E, payload: EmitPayloadOf<E>): boolean {
    const contract = SOCKET_EVENTS[event];
    const schema = contract.emitSchema;
    // Defense-in-depth against a future catalogue-authoring mistake (an
    // emit-capable entry left without an emitSchema): the `false` branch is
    // unreachable through the typed public API today — proven by
    // events.test.ts's "every emit-capable event has a non-null
    // emitSchema" invariant check — but a runtime guard here still beats a
    // crash if that invariant is ever violated (§5.5: never throw).
    /* v8 ignore next 3 */
    if (schema) {
      const parsed = schema.safeParse(payload);
      if (!parsed.success) warnPayloadMismatch('emit', event, parsed.error.issues);
    }
    // socket.io-client's Socket.emit returns `this` (chaining), not a
    // boolean — matches the old app's own `!!socket?.emit(...)` coercion
    // (hooks/useSocket.jsx:42): "emitted" means "a socket existed to write
    // to", not a delivery acknowledgement.
    socket.emit(event, payload);
    return true;
  }

  function on<E extends ReceivableEventName>(event: E, handler: (payload: ReceivePayloadOf<E>) => void): void {
    const contract = SOCKET_EVENTS[event];
    const schema = contract.receiveSchema;
    const wrapped = (payload: unknown): void => {
      // Same defense-in-depth rationale as emit()'s guard above — proven
      // unreachable for typed callers by events.test.ts.
      /* v8 ignore next 3 */
      if (schema) {
        const parsed = schema.safeParse(payload);
        if (!parsed.success) warnPayloadMismatch('receive', event, parsed.error.issues);
      }
      handler(payload as ReceivePayloadOf<E>);
    };
    wrappedListeners.set(handler, wrapped);
    // String(event), not `event as string`: `event`'s generic literal-union
    // type defeats socket.io-client's conditional ReservedOrUserListener
    // resolution (it can't distribute over an unresolved generic) even
    // though every concrete instantiation is a plain application event name
    // distinct from SocketReservedEvents — verified safe against the
    // installed socket.io-client@4.8.3 typings (DefaultEventsMap's string
    // index signature accepts any string event name with an untyped
    // listener). A coercion, not a type assertion, so it satisfies both tsc
    // and tsgolint's no-unnecessary-type-assertion (which only targets `as`).
    socket.on(String(event), wrapped);
  }

  function off<E extends ReceivableEventName>(event: E, handler: (payload: ReceivePayloadOf<E>) => void): void {
    const wrapped = wrappedListeners.get(handler);
    if (wrapped) {
      socket.off(String(event), wrapped);
      wrappedListeners.delete(handler);
    }
  }

  return {
    socket,
    getConnectionState: () => useConnectionStore.getState().status,
    useConnectionState: () => useConnectionStore((s) => s.status),
    emit,
    on,
    off,
    disconnect: () => socket.disconnect(),
  };
}
