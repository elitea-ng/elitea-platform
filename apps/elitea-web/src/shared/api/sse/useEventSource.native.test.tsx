/**
 * useEventSource under a native transport (ADR-0029): the factory replaces the
 * browser constructor, callers are untouched, and the handlers still fire.
 */
import { render } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { setNativeTransport, type EventSourceLike, type NativeTransport } from '../nativeTransport';

import { useEventSource } from './useEventSource';

function Probe({ onPing }: { onPing: (e: MessageEvent) => void }): null {
  useEventSource('https://h.example/api/v2/stream', { ping: onPing });
  return null;
}

afterEach(() => setNativeTransport(undefined));

describe('useEventSource with a native transport', () => {
  it('opens through the transport factory (no EventSource constructor needed) and closes on unmount', () => {
    const listeners = new Map<string, (e: MessageEvent) => void>();
    const close = vi.fn();
    const source: EventSourceLike = {
      readyState: 0,
      addEventListener: (type, listener) => void listeners.set(type, listener),
      close,
    };
    const createEventSource = vi.fn<(url: string) => EventSourceLike>().mockReturnValue(source);
    setNativeTransport({ createEventSource } as unknown as NativeTransport);
    const onPing = vi.fn();

    const view = render(<Probe onPing={onPing} />);

    expect(createEventSource).toHaveBeenCalledWith('https://h.example/api/v2/stream');
    listeners.get('ping')?.(new MessageEvent('ping', { data: 'x' }));
    expect(onPing).toHaveBeenCalledTimes(1);
    view.unmount();
    expect(close).toHaveBeenCalledTimes(1);
  });
});
