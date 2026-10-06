import { act, render } from '@testing-library/react-native';
import { AccessibilityInfo, Text } from 'react-native';

import { EventStreamParser } from '@/api/sse/parser';
import { createAnnouncer } from '@/features/a11y/announce';
import { modalAnimation, useReducedMotion } from '@/features/a11y/motion';
import { turnAnnouncement, useTurnAnnouncements } from '@/features/a11y/turnAnnouncements';
import { loadFixtureBytes, type FixtureName } from '@/features/chat/__fixtures__/loadFixture';
import { parseFrame } from '@/features/chat/frames';
import type { LiveTurn } from '@/features/chat/turnController';
import { INITIAL_TURN, isTurnTerminal, reduceTurn, type TurnState } from '@/features/chat/turnReducer';

function play(name: FixtureName): TurnState {
  const parser = new EventStreamParser();
  let state = INITIAL_TURN;
  for (const event of [...parser.push(loadFixtureBytes(name)), ...parser.end()]) {
    const frame = parseFrame(event);
    state = reduceTurn(state, frame);
    if (isTurnTerminal(frame)) break;
  }
  return state;
}
const streaming = { ...INITIAL_TURN, phase: 'streaming' as const };
const ctx = { author: 'Zefir Tool Agent', regeneration: false };

describe('turn announcements', () => {
  it('says once that the answer is done, stopped, regenerated or failed', () => {
    expect(turnAnnouncement(streaming, play('simple-answer'), ctx)).toBe('Zefir Tool Agent answered.');
    expect(turnAnnouncement(streaming, play('cancelled'), ctx)).toBe('Answer stopped.');
    expect(turnAnnouncement(streaming, play('regenerate-answer'), { ...ctx, regeneration: true })).toBe('New answer ready.');
    expect(turnAnnouncement(streaming, play('execution-failed'), ctx)).toBe('The answer failed. The execution deadline was exceeded.');
    // No change of phase, nothing to say (a delta is never announced).
    expect(turnAnnouncement(streaming, { ...streaming, error: null, pause: null, stopped: false }, ctx)).toBeNull();
  });

  it('says what a pause waits for (the captured Python sensitive-tool pause)', () => {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const captured = require('../chat/__fixtures__/captured/python-sensitive-tool-pause.json') as { frames: Record<string, unknown>[] };
    let state: TurnState = streaming;
    for (const node of captured.frames) state = reduceTurn(state, { kind: 'node', node: node as never });
    expect(state.phase).toBe('paused');
    expect(turnAnnouncement(streaming, state, ctx)).toBe('Approval needed to run mock_tool_status.');
    const auth = { ...streaming, phase: 'paused' as const, pause: { kind: 'auth' as const, requests: [], threadId: null, messageId: null } };
    expect(turnAnnouncement(streaming, auth, ctx)).toBe('A toolkit needs you to sign in before the agent can go on.');
    // A pause the app cannot read still says the agent waits.
    expect(turnAnnouncement(streaming, play('hitl-terminal'), ctx)).toBe('The agent is waiting for you.');
  });
});

describe('announcer', () => {
  function harness() {
    const said: string[] = [];
    let clock = 0;
    const timers: { fn: () => void; at: number; id: number }[] = [];
    let ids = 0;
    const announcer = createAnnouncer({
      announce: (text) => said.push(text),
      now: () => clock,
      setTimer: (fn, ms) => {
        const id = ++ids;
        timers.push({ fn, at: clock + ms, id });
        return id;
      },
      clearTimer: (id) => {
        const i = timers.findIndex((t) => t.id === id);
        if (i >= 0) timers.splice(i, 1);
      },
    });
    const advance = (ms: number) => {
      clock += ms;
      for (const t of timers.filter((t) => t.at <= clock)) {
        timers.splice(timers.indexOf(t), 1);
        t.fn();
      }
    };
    return { announcer, said, advance };
  }

  it('debounces: the last text within the window wins', () => {
    const h = harness();
    h.announcer.say('one');
    h.advance(100);
    h.announcer.say('two');
    h.advance(599);
    expect(h.said).toEqual([]);
    h.advance(1);
    expect(h.said).toEqual(['two']);
  });

  it('does not repeat the same text soon after, and cancel drops a pending one', () => {
    const h = harness();
    h.announcer.say('Answer ready.');
    h.advance(600);
    h.announcer.say('Answer ready.');
    h.advance(600);
    expect(h.said).toEqual(['Answer ready.']);
    h.advance(5000);
    h.announcer.say('Answer ready.');
    h.advance(600);
    expect(h.said).toEqual(['Answer ready.', 'Answer ready.']);
    h.announcer.say('gone');
    h.announcer.cancel();
    h.advance(1000);
    expect(h.said).toHaveLength(2);
  });
});

describe('useTurnAnnouncements', () => {
  function Probe({ live }: { live: LiveTurn | null }) {
    useTurnAnnouncements(live, 'Zefir Helper');
    return <Text>probe</Text>;
  }
  const live = (turn: Partial<TurnState>): LiveTurn =>
    ({ questionId: 'q1', turn: { ...INITIAL_TURN, ...turn }, regeneration: null }) as unknown as LiveTurn;

  it('announces the phase change once, not every delta, through VoiceOver', async () => {
    jest.useFakeTimers();
    const spy = jest.spyOn(AccessibilityInfo, 'announceForAccessibility').mockImplementation(() => {});
    // React Native's jest setup already mocks it: clear what an earlier test recorded.
    spy.mockClear();
    const view = await render(<Probe live={live({ phase: 'sending' })} />);
    await view.rerender(<Probe live={live({ phase: 'streaming', content: 'a' })} />);
    await view.rerender(<Probe live={live({ phase: 'streaming', content: 'ab' })} />);
    await view.rerender(<Probe live={live({ phase: 'done', content: 'abc' })} />);
    await act(async () => {
      jest.advanceTimersByTime(1000);
    });
    expect(spy.mock.calls).toEqual([['Zefir Helper answered.']]);
    await view.unmount();
    jest.useRealTimers();
  });

  it('a turn found already finished on mount says nothing', async () => {
    jest.useFakeTimers();
    const spy = jest.spyOn(AccessibilityInfo, 'announceForAccessibility').mockImplementation(() => {});
    // React Native's jest setup already mocks it: clear what an earlier test recorded.
    spy.mockClear();
    const view = await render(<Probe live={live({ phase: 'done' })} />);
    await act(async () => {
      jest.advanceTimersByTime(1000);
    });
    expect(spy).not.toHaveBeenCalled();
    await view.unmount();
    jest.useRealTimers();
  });
});

describe('reduced motion', () => {
  it('a slide becomes a fade; other animations stay', () => {
    expect(modalAnimation('slide', true)).toBe('fade');
    expect(modalAnimation('fade', true)).toBe('fade');
    expect(modalAnimation('slide', false)).toBe('slide');
  });

  it('follows the system setting while the app runs', async () => {
    let listener: ((value: boolean) => void) | null = null;
    jest.spyOn(AccessibilityInfo, 'isReduceMotionEnabled').mockResolvedValue(false);
    jest.spyOn(AccessibilityInfo, 'addEventListener').mockImplementation(((name: string, fn: (value: boolean) => void) => {
      if (name === 'reduceMotionChanged') listener = fn;
      return { remove: () => {} };
    }) as never);
    function Probe() {
      return <Text>{useReducedMotion() ? 'reduced' : 'full'}</Text>;
    }
    const view = await render(<Probe />);
    expect(view.getByText('full')).toBeTruthy();
    await act(async () => listener?.(true));
    expect(view.getByText('reduced')).toBeTruthy();
  });
});

afterEach(() => jest.restoreAllMocks());
