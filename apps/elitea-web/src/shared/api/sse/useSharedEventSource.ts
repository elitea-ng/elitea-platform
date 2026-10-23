/**
 * `useSharedEventSource` — one `EventSource` per URL for the whole tab,
 * shared by every component that follows it.
 *
 * `useEventSource` opens one connection per caller. That is right for a
 * stream only one component reads (an execution's events), and wrong for the
 * project event stream (GET /events/prompt_lib/{projectId}), which the chat
 * rail, the sidebar, the Cloud executor notice and the desktop's Local work
 * section all follow at once: each caller cost one connection of the
 * server's per-principal admission cap, and each applied every frame again.
 *
 * Here the callers of one URL share one connection, opened by the first and
 * closed with the last. Each caller subscribes under a CHANNEL name: the
 * callers of one channel do the same work with a frame (apply it to the same
 * store), so a frame runs the handler of one of them only (the earliest
 * still mounted). Different channels each get every frame. `onError` and
 * `onOpen` reach every caller: they observe the connection, not a frame.
 *
 * Degrades as `useEventSource` does: a falsy URL, or a runtime without an
 * `EventSource` and without a native transport, opens nothing.
 */
import { useEffect, useRef } from 'react';

import { getNativeTransport, type EventSourceLike } from '../nativeTransport';

type Handlers = Readonly<Record<string, (event: MessageEvent) => void>>;

interface SharedOptions {
  /** As `useEventSource`'s: `readyState` 2 (CLOSED) means the connection failed for good. */
  readonly onError?: ((event: Event, readyState: number) => void) | undefined;
  readonly onOpen?: ((event: Event) => void) | undefined;
}

interface Subscriber {
  readonly channel: string;
  readonly handlers: { current: Handlers };
  readonly options: { current: SharedOptions };
}

interface SharedSource {
  readonly source: EventSourceLike;
  /** Mount order: the first subscriber of a channel handles its frames. */
  readonly subscribers: Subscriber[];
  /** The event names the source already listens to. */
  readonly listening: Set<string>;
}

const sources = new Map<string, SharedSource>();

// The loops below iterate over a copy of the subscribers: a handler may
// unmount a component, which removes its subscriber meanwhile.

function openSource(url: string): SharedSource | null {
  const nativeFactory = getNativeTransport()?.createEventSource;
  if (nativeFactory === undefined && typeof EventSource === 'undefined') return null;
  const source: EventSourceLike = nativeFactory?.(url) ?? new EventSource(url, { withCredentials: true });
  const shared: SharedSource = { source, subscribers: [], listening: new Set() };
  source.addEventListener('open', (event) => {
    for (const subscriber of shared.subscribers.slice()) subscriber.options.current.onOpen?.(event);
  });
  source.addEventListener('error', (event) => {
    for (const subscriber of shared.subscribers.slice()) subscriber.options.current.onError?.(event, source.readyState);
  });
  return shared;
}

function listen(shared: SharedSource, name: string): void {
  if (shared.listening.has(name)) return;
  shared.listening.add(name);
  shared.source.addEventListener(name, (event) => {
    const handled = new Set<string>();
    for (const subscriber of shared.subscribers.slice()) {
      const handler = subscriber.handlers.current[name];
      if (handler === undefined || handled.has(subscriber.channel)) continue;
      handled.add(subscriber.channel);
      handler(event);
    }
  });
}

/**
 * Follow `url` under `channel` for the lifetime of the calling component,
 * on the tab's one connection to it. Handlers are read live, as in
 * `useEventSource`; the subscription changes only with `url`, `channel` or
 * the set of event names.
 */
export function useSharedEventSource(url: string | null | undefined, channel: string, handlers: Handlers, options: SharedOptions = {}): void {
  const handlersRef = useRef(handlers);
  const optionsRef = useRef(options);
  useEffect(() => {
    handlersRef.current = handlers;
    optionsRef.current = options;
  });
  const eventNamesKey = Object.keys(handlers).sort().join('\u0000');

  useEffect(() => {
    if (!url) return undefined;
    const shared = sources.get(url) ?? openSource(url);
    if (shared === null) return undefined;
    sources.set(url, shared);
    const subscriber: Subscriber = { channel, handlers: handlersRef, options: optionsRef };
    shared.subscribers.push(subscriber);
    for (const name of eventNamesKey === '' ? [] : eventNamesKey.split('\u0000')) listen(shared, name);
    return () => {
      const index = shared.subscribers.indexOf(subscriber);
      if (index !== -1) shared.subscribers.splice(index, 1);
      if (shared.subscribers.length === 0) {
        shared.source.close();
        if (sources.get(url) === shared) sources.delete(url);
      }
    };
  }, [url, channel, eventNamesKey]);
}
