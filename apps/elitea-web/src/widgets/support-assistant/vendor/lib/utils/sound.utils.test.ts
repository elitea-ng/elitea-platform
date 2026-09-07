/**
 * `playPopupSound` — the two-note chime behind a proactive nudge.
 *
 * jsdom ships no `AudioContext`, so every branch here installs a small double
 * and asserts the SHAPE of calls it makes, not real audio. The function must
 * never throw, whatever the double does — a chime failing must never break
 * the popup it decorates.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { playPopupSound } from './sound.utils';

function installFakeAudioContext(initialState: 'running' | 'suspended' = 'running') {
  const oscillators: Array<{ start: ReturnType<typeof vi.fn>; stop: ReturnType<typeof vi.fn>; onended: (() => void) | null }> = [];
  let closed = false;

  class FakeAudioContext {
    state = initialState;
    currentTime = 0;
    destination = {};

    createOscillator() {
      const osc = {
        type: '',
        frequency: { value: 0 },
        connect: vi.fn(),
        start: vi.fn(),
        stop: vi.fn(),
        onended: null as (() => void) | null,
      };
      oscillators.push(osc);
      return osc;
    }

    createGain() {
      return {
        connect: vi.fn(),
        gain: { setValueAtTime: vi.fn(), linearRampToValueAtTime: vi.fn(), exponentialRampToValueAtTime: vi.fn() },
      };
    }

    resume() {
      this.state = 'running';
      return Promise.resolve();
    }

    close() {
      closed = true;
      return Promise.resolve();
    }
  }

  const globals = globalThis as unknown as Record<string, unknown>;
  globals['AudioContext'] = FakeAudioContext;

  return { oscillators, isClosed: () => closed };
}

afterEach(() => {
  vi.restoreAllMocks();
  delete (globalThis as unknown as Record<string, unknown>)['AudioContext'];
});

describe('playPopupSound', () => {
  it('plays two notes and closes the context once the second one ends', () => {
    const { oscillators, isClosed } = installFakeAudioContext('running');

    playPopupSound();

    expect(oscillators).toHaveLength(2);
    expect(oscillators[0]?.start).toHaveBeenCalled();
    expect(oscillators[1]?.stop).toHaveBeenCalled();

    // The chime closes the context from the SECOND note's `onended`.
    oscillators[1]?.onended?.();
    expect(isClosed()).toBe(true);
  });

  it('resumes a SUSPENDED context before playing', async () => {
    const { oscillators } = installFakeAudioContext('suspended');

    playPopupSound();
    // `resume()` returns a promise; the notes play in its `.then`.
    await Promise.resolve();
    await Promise.resolve();

    expect(oscillators).toHaveLength(2);
  });

  it('closes without playing when the context never reaches "running"', () => {
    const closeSpy = vi.fn().mockResolvedValue(undefined);
    class NeverRunningAudioContext {
      state = 'closed';
      currentTime = 0;
      close = closeSpy;
    }
    const globals = globalThis as unknown as Record<string, unknown>;
    globals['AudioContext'] = NeverRunningAudioContext;

    expect(() => playPopupSound()).not.toThrow();
    expect(closeSpy).toHaveBeenCalledOnce();
  });

  it('swallows the error silently when AudioContext is unavailable', () => {
    delete (globalThis as unknown as Record<string, unknown>)['AudioContext'];
    expect(() => playPopupSound()).not.toThrow();
  });
});
