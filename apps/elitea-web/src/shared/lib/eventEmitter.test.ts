import { describe, expect, it, vi } from 'vitest';

import { eventEmitter } from './eventEmitter';

describe('eventEmitter', () => {
  it('calls every listener registered for an event, with the emitted data', () => {
    const first = vi.fn();
    const second = vi.fn();
    eventEmitter.on('TestEvent', first);
    eventEmitter.on('TestEvent', second);

    eventEmitter.emit('TestEvent', 'payload');

    expect(first).toHaveBeenCalledWith('payload');
    expect(second).toHaveBeenCalledWith('payload');

    eventEmitter.off('TestEvent', first);
    eventEmitter.off('TestEvent', second);
  });

  it('does nothing when emitting an event with no listeners', () => {
    expect(() => eventEmitter.emit('NoListenersEvent')).not.toThrow();
  });

  it('off removes only the specified listener', () => {
    const kept = vi.fn();
    const removed = vi.fn();
    eventEmitter.on('OffEvent', kept);
    eventEmitter.on('OffEvent', removed);

    eventEmitter.off('OffEvent', removed);
    eventEmitter.emit('OffEvent');

    expect(kept).toHaveBeenCalledTimes(1);
    expect(removed).not.toHaveBeenCalled();

    eventEmitter.off('OffEvent', kept);
  });

  it('off on an event with no listeners is a no-op', () => {
    expect(() => eventEmitter.off('NeverRegistered', vi.fn())).not.toThrow();
  });
});
