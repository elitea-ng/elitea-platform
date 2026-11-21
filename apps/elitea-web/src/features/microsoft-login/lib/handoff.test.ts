import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { MESSAGE_TYPE, outcomeOf, waitForHandoff } from './handoff';

const STATE = 'state-1';

function post(data: unknown, init: MessageEventInit = {}): void {
  window.dispatchEvent(new MessageEvent('message', { data, origin: window.location.origin, ...init }));
}

describe('outcomeOf', () => {
  it('reads a handoff for the state it was asked about', () => {
    expect(outcomeOf({ type: MESSAGE_TYPE, state: STATE, handoff: 'h1' }, STATE)).toEqual({ kind: 'handoff', handoff: 'h1' });
  });

  it('reads an error code the callback page reported', () => {
    expect(outcomeOf({ type: MESSAGE_TYPE, state: STATE, error: 'denied' }, STATE)).toEqual({ kind: 'error', error: 'denied' });
  });

  it('ignores another sign-in state, another type, and anything that is not an object', () => {
    expect(outcomeOf({ type: MESSAGE_TYPE, state: 'other', handoff: 'h1' }, STATE)).toBeUndefined();
    expect(outcomeOf({ type: 'something-else', state: STATE, handoff: 'h1' }, STATE)).toBeUndefined();
    expect(outcomeOf('handoff', STATE)).toBeUndefined();
    expect(outcomeOf(null, STATE)).toBeUndefined();
  });

  it('ignores a message with neither a handoff nor an error', () => {
    expect(outcomeOf({ type: MESSAGE_TYPE, state: STATE }, STATE)).toBeUndefined();
    expect(outcomeOf({ type: MESSAGE_TYPE, state: STATE, handoff: '' }, STATE)).toBeUndefined();
  });
});

class FakeChannel {
  static instances: FakeChannel[] = [];
  onmessage: ((event: MessageEvent) => void) | null = null;
  closed = false;
  readonly name: string;
  constructor(name: string) {
    this.name = name;
    FakeChannel.instances.push(this);
  }
  close(): void {
    this.closed = true;
  }
}

describe('waitForHandoff', () => {
  beforeEach(() => {
    FakeChannel.instances = [];
    vi.stubGlobal('BroadcastChannel', FakeChannel);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  const params = { state: STATE, channelName: 'elitea-microsoft-login', timeoutMs: 60_000, popup: null };

  it('resolves on a same-origin postMessage for its state', async () => {
    const wait = waitForHandoff(params);
    post({ type: MESSAGE_TYPE, state: STATE, handoff: 'via-message' });
    await expect(wait.outcome).resolves.toEqual({ kind: 'handoff', handoff: 'via-message' });
  });

  it('resolves on the BroadcastChannel when the opener was severed, and closes the channel', async () => {
    const wait = waitForHandoff(params);
    const channel = FakeChannel.instances[0];
    expect(channel?.name).toBe('elitea-microsoft-login');
    channel?.onmessage?.({ data: { type: MESSAGE_TYPE, state: STATE, handoff: 'via-channel' } } as MessageEvent);
    await expect(wait.outcome).resolves.toEqual({ kind: 'handoff', handoff: 'via-channel' });
    expect(channel?.closed).toBe(true);
  });

  it('ignores a message of another sign-in and waits for its own', async () => {
    const wait = waitForHandoff(params);
    post({ type: MESSAGE_TYPE, state: 'someone-elses', handoff: 'foreign' });
    post({ type: MESSAGE_TYPE, state: STATE, handoff: 'mine' });
    await expect(wait.outcome).resolves.toEqual({ kind: 'handoff', handoff: 'mine' });
  });

  it('ignores a message from another origin', async () => {
    const wait = waitForHandoff(params);
    post({ type: MESSAGE_TYPE, state: STATE, handoff: 'cross-origin' }, { origin: 'https://evil.example' });
    post({ type: MESSAGE_TYPE, state: STATE, handoff: 'same-origin' });
    await expect(wait.outcome).resolves.toEqual({ kind: 'handoff', handoff: 'same-origin' });
  });

  it('ignores a message from a window that is not the popup it opened', async () => {
    const popup = { name: 'the popup' } as unknown as Window;
    const wait = waitForHandoff({ ...params, popup });
    // `window` is a real Window that is not the popup.
    post({ type: MESSAGE_TYPE, state: STATE, handoff: 'from-another-window' }, { source: window });
    post({ type: MESSAGE_TYPE, state: STATE, handoff: 'from-the-popup-or-channel' });
    await expect(wait.outcome).resolves.toEqual({ kind: 'handoff', handoff: 'from-the-popup-or-channel' });
  });

  it('rejects with expired when the sign-in runs out of time', async () => {
    vi.useFakeTimers();
    const wait = waitForHandoff({ ...params, timeoutMs: 5_000 });
    const rejected = expect(wait.outcome).rejects.toThrow('expired');
    await vi.advanceTimersByTimeAsync(5_001);
    await rejected;
  });

  it('rejects with cancelled when aborted, and stops listening', async () => {
    const controller = new AbortController();
    const wait = waitForHandoff({ ...params, signal: controller.signal });
    controller.abort();
    await expect(wait.outcome).rejects.toThrow('cancelled');
    expect(FakeChannel.instances[0]?.closed).toBe(true);
  });

  it('works without a BroadcastChannel (postMessage alone)', async () => {
    vi.stubGlobal('BroadcastChannel', undefined);
    const wait = waitForHandoff(params);
    post({ type: MESSAGE_TYPE, state: STATE, error: 'tenant_mismatch' });
    await expect(wait.outcome).resolves.toEqual({ kind: 'error', error: 'tenant_mismatch' });
  });
});
