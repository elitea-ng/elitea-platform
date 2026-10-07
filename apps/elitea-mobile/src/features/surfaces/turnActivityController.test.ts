import type { AppStateStatus } from 'react-native';

import type { LiveTurn } from '@/features/chat/turnController';
import { INITIAL_TURN, type TurnState } from '@/features/chat/turnReducer';
import {
  DONE_LINGER_MS,
  START_AFTER_MS,
  TurnActivityController,
  WAITING_LINGER_MS,
  type ActivityFactory,
  type TurnHandle,
} from '@/features/surfaces/turnActivityController';
import { APPROXIMATE_TEXT, GENERIC_TITLE, type TurnActivityProps } from '@/features/surfaces/turnActivityModel';
import type { NotificationPreview } from '@/security/policy';

const KEY = '7:0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e';

class FakeTurn implements TurnHandle {
  state: LiveTurn;
  private listeners = new Set<() => void>();
  constructor(turn: Partial<TurnState> = {}) {
    this.state = {
      questionId: 'q',
      userText: 'secret question text',
      turn: { ...INITIAL_TURN, ...turn },
      execution: null,
      streamStatus: null,
      canReconnect: false,
      failure: null,
      settled: false,
      queued: false,
      continuation: false,
      attachmentNames: [],
      regeneration: null,
    };
  }
  getSnapshot = () => this.state;
  subscribe = (l: () => void) => {
    this.listeners.add(l);
    return () => this.listeners.delete(l);
  };
  set(turn: Partial<TurnState>, extra: Partial<LiveTurn> = {}) {
    this.state = { ...this.state, ...extra, turn: { ...this.state.turn, ...turn } };
    for (const l of [...this.listeners]) l();
  }
}

interface Fake {
  props: TurnActivityProps;
  url?: string;
  updates: { props: TurnActivityProps; stale?: Date }[];
  ended: { policy?: unknown; props?: TurnActivityProps } | null;
}

function setup(opts: { preview?: NotificationPreview; title?: string | null; leftovers?: number } = {}) {
  let now = 1_000_000;
  const timers: { at: number; fn: () => void; id: number }[] = [];
  let nextId = 1;
  const started: Fake[] = [];
  const leftovers: Fake[] = Array.from({ length: opts.leftovers ?? 0 }, () => ({ props: {} as TurnActivityProps, updates: [], ended: null }));
  const handle = (fake: Fake) => ({
    update: jest.fn(async (props: TurnActivityProps, stale?: Date) => {
      fake.updates.push({ props, stale });
      fake.props = props;
    }),
    end: jest.fn(async (policy?: unknown, props?: TurnActivityProps) => {
      fake.ended = { policy, props };
    }),
  });
  const factory: ActivityFactory = {
    start: (props, url) => {
      const fake: Fake = { props, url, updates: [], ended: null };
      started.push(fake);
      return handle(fake);
    },
    getInstances: () => leftovers.map(handle),
  };
  const registry = new Map<string, FakeTurn>();
  const registryListeners = new Set<() => void>();
  let appState: AppStateStatus = 'active';
  const appListeners = new Set<(s: AppStateStatus) => void>();
  let preview: NotificationPreview = opts.preview ?? 'none';
  const previewListeners = new Set<() => void>();
  const controller = new TurnActivityController({
    factory,
    turns: { list: () => [...registry.entries()], subscribe: (l) => (registryListeners.add(l), () => registryListeners.delete(l)) },
    appState: { current: () => appState, subscribe: (l) => (appListeners.add(l), () => appListeners.delete(l)) },
    preview: { get: () => preview, subscribe: (l) => (previewListeners.add(l), () => previewListeners.delete(l)) },
    workspaceId: () => 'aaaaaaaaaaaa',
    titleFor: () => (opts.title === undefined ? 'Contract review: Acme' : opts.title),
    now: () => now,
    setTimer: (fn, ms) => {
      const id = nextId++;
      timers.push({ at: now + ms, fn, id });
      return id;
    },
    clearTimer: (id) => {
      const i = timers.findIndex((t) => t.id === id);
      if (i >= 0) timers.splice(i, 1);
    },
  });
  return {
    controller,
    started,
    leftovers,
    add(turn: FakeTurn, key = KEY) {
      registry.set(key, turn);
      for (const l of [...registryListeners]) l();
    },
    remove(key = KEY) {
      registry.delete(key);
      for (const l of [...registryListeners]) l();
    },
    advance(ms: number) {
      now += ms;
      for (const t of timers.filter((x) => x.at <= now).sort((a, b) => a.at - b.at)) {
        timers.splice(timers.indexOf(t), 1);
        t.fn();
      }
    },
    setApp(state: AppStateStatus) {
      appState = state;
      for (const l of [...appListeners]) l(state);
    },
    setPreview(next: NotificationPreview) {
      preview = next;
      for (const l of [...previewListeners]) l();
    },
    get now() {
      return now;
    },
  };
}

it('starts only once a turn has run for five seconds, with a link to its thread', () => {
  const env = setup();
  env.controller.start();
  const turn = new FakeTurn({ phase: 'sending' });
  env.add(turn);
  env.advance(START_AFTER_MS - 1);
  expect(env.started).toHaveLength(0);
  env.advance(1);
  expect(env.started).toHaveLength(1);
  expect(env.started[0]!.url).toBe('agentzefir://surface/c/0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e?ws=aaaaaaaaaaaa&p=7');
  expect(env.started[0]!.props).toMatchObject({ phase: 'working', title: GENERIC_TITLE, approximate: false });
});

it('a short answer never gets one', () => {
  const env = setup();
  env.controller.start();
  const turn = new FakeTurn({ phase: 'streaming', content: 'Hi' });
  env.add(turn);
  env.advance(2000);
  turn.set({ phase: 'done' });
  env.advance(10_000);
  expect(env.started).toHaveLength(0);
});

it('generic text by default: no title, no message text, no tool name', () => {
  const env = setup({ preview: 'none' });
  env.controller.start();
  const turn = new FakeTurn({ phase: 'streaming' });
  env.add(turn);
  env.advance(START_AFTER_MS);
  turn.set({ tools: [{ id: 't1', name: 'sharepoint_search', displayName: 'SharePoint', toolkit: 'sp', parentAgent: null, status: 'running', startedAt: 1, endedAt: null } as never] });
  const shown = JSON.stringify([env.started[0]!.props, ...env.started[0]!.updates.map((u) => u.props)]);
  expect(env.started[0]!.updates.at(-1)!.props).toMatchObject({ phase: 'tool', detail: 'Using a tool…' });
  expect(shown).not.toMatch(/Contract|secret question|sharepoint|SharePoint/);
});

it("shows the conversation's title only when the workspace lets notifications show titles", () => {
  const env = setup({ preview: 'title' });
  env.controller.start();
  env.add(new FakeTurn({ phase: 'streaming' }));
  env.advance(START_AFTER_MS);
  expect(env.started[0]!.props.title).toBe('Contract review: Acme');
  env.setPreview('none');
  expect(env.started[0]!.updates.at(-1)!.props.title).toBe(GENERIC_TITLE);
});

it('updates on a phase change, not on every streamed frame', () => {
  const env = setup();
  env.controller.start();
  const turn = new FakeTurn({ phase: 'streaming' });
  env.add(turn);
  env.advance(START_AFTER_MS);
  turn.set({ content: 'a' });
  turn.set({ content: 'ab' });
  turn.set({ content: 'abc' });
  expect(env.started[0]!.updates.map((u) => u.props.phase)).toEqual(['writing']);
});

it('ends when the answer finishes, fails or pauses for a decision', () => {
  const env = setup();
  env.controller.start();
  const done = new FakeTurn({ phase: 'streaming' });
  env.add(done);
  env.advance(START_AFTER_MS);
  done.set({ phase: 'done', content: 'answer' });
  expect(env.started[0]!.ended).toMatchObject({ props: { phase: 'done', detail: 'Answer ready' } });
  expect((env.started[0]!.ended!.policy as { after: Date }).after.getTime()).toBe(env.now + DONE_LINGER_MS);

  const paused = new FakeTurn({ phase: 'streaming' });
  env.add(paused, '7:aaaaaaaa-0000-4000-8000-000000000002');
  env.advance(START_AFTER_MS);
  paused.set({ phase: 'paused', pause: { kind: 'hitl', shape: 'single', interrupts: [], threadId: null, messageId: 'm' } as never });
  expect(env.started[1]!.ended).toMatchObject({ props: { phase: 'waiting', detail: 'Waiting for your decision' } });
  expect((env.started[1]!.ended!.policy as { after: Date }).after.getTime()).toBe(env.now + WAITING_LINGER_MS);
});

it('on suspend it is marked approximate and stale, and live again on return', () => {
  const env = setup();
  env.controller.start();
  env.add(new FakeTurn({ phase: 'streaming' }));
  env.advance(START_AFTER_MS);
  env.setApp('background');
  const approx = env.started[0]!.updates.at(-1)!;
  expect(approx.props).toMatchObject({ approximate: true, detail: APPROXIMATE_TEXT });
  expect(approx.stale?.getTime()).toBe(env.now);
  env.setApp('active');
  expect(env.started[0]!.updates.at(-1)!.props).toMatchObject({ approximate: false, detail: 'Working…' });
});

it('never starts while the app is in the background, and does once it is back', () => {
  const env = setup();
  env.controller.start();
  env.setApp('background');
  env.add(new FakeTurn({ phase: 'streaming' }));
  env.advance(START_AFTER_MS * 2);
  expect(env.started).toHaveLength(0);
  env.setApp('active');
  env.advance(0);
  expect(env.started).toHaveLength(1);
});

it('ends at once when the turn leaves memory (thread closed, wipe, switch) and on endAll', () => {
  const env = setup();
  env.controller.start();
  env.add(new FakeTurn({ phase: 'streaming' }));
  env.advance(START_AFTER_MS);
  env.remove();
  expect(env.started[0]!.ended).toEqual({ policy: 'immediate', props: undefined });

  env.add(new FakeTurn({ phase: 'streaming' }));
  env.advance(START_AFTER_MS);
  env.controller.endAll();
  expect(env.started[1]!.ended?.policy).toBe('immediate');
});

it('a new process ends what a previous one left running', () => {
  const env = setup({ leftovers: 2 });
  env.controller.start();
  expect(env.leftovers.map((l) => l.ended?.policy)).toEqual(['immediate', 'immediate']);
});

it('a queued (offline) turn gets none', () => {
  const env = setup();
  env.controller.start();
  const turn = new FakeTurn({ phase: 'sending' });
  env.add(turn);
  turn.set({}, { queued: true });
  env.advance(START_AFTER_MS * 2);
  expect(env.started).toHaveLength(0);
});

it('caps the number of activities', () => {
  const env = setup();
  env.controller.start();
  for (let i = 1; i <= 3; i++) env.add(new FakeTurn({ phase: 'streaming' }), `7:aaaaaaaa-0000-4000-8000-00000000000${i}`);
  env.advance(START_AFTER_MS);
  expect(env.started).toHaveLength(2);
});

it('a finished activity still lingering on the lock screen ends at once on endAll (logout, revocation, switch)', () => {
  const env = setup({ preview: 'title' });
  env.controller.start();
  const paused = new FakeTurn({ phase: 'streaming' });
  env.add(paused);
  env.advance(START_AFTER_MS);
  paused.set({ phase: 'paused', pause: { kind: 'hitl', shape: 'single', interrupts: [], threadId: null, messageId: 'm' } as never });
  expect((env.started[0]!.ended!.policy as { after: Date }).after.getTime()).toBe(env.now + WAITING_LINGER_MS);
  env.controller.endAll();
  expect(env.started[0]!.ended?.policy).toBe('immediate');
});

it('a lingering activity ends at once when its turn leaves memory', () => {
  const env = setup();
  env.controller.start();
  const done = new FakeTurn({ phase: 'streaming' });
  env.add(done);
  env.advance(START_AFTER_MS);
  done.set({ phase: 'done', content: 'answer' });
  env.remove();
  expect(env.started[0]!.ended?.policy).toBe('immediate');
});

const PAUSE = { phase: 'paused', pause: { kind: 'hitl', shape: 'single', interrupts: [], threadId: null, messageId: 'm' } } as never;

it('answering a pause (a continuation) ends the stale "needs you" activity before a new one starts', () => {
  const env = setup();
  env.controller.start();
  const paused = new FakeTurn({ phase: 'streaming' });
  env.add(paused);
  env.advance(START_AFTER_MS);
  paused.set(PAUSE);
  env.add(new FakeTurn({ phase: 'sending' }));
  expect(env.started[0]!.ended?.policy).toBe('immediate');
  env.advance(START_AFTER_MS);
  expect(env.started).toHaveLength(2);
});

it('the same turn running again ends its lingering activity: one per conversation', () => {
  const env = setup();
  env.controller.start();
  const turn = new FakeTurn({ phase: 'streaming' });
  env.add(turn);
  env.advance(START_AFTER_MS);
  turn.set(PAUSE);
  turn.set({ phase: 'streaming', pause: null } as never);
  expect(env.started[0]!.ended?.policy).toBe('immediate');
  env.advance(START_AFTER_MS);
  expect(env.started).toHaveLength(2);
  expect(env.started[1]!.ended).toBeNull();
});

it('lingering activities count towards the cap until their dismissal date', () => {
  const env = setup();
  env.controller.start();
  const a = new FakeTurn({ phase: 'streaming' });
  const b = new FakeTurn({ phase: 'streaming' });
  env.add(a, '7:aaaaaaaa-0000-4000-8000-000000000001');
  env.add(b, '7:aaaaaaaa-0000-4000-8000-000000000002');
  env.advance(START_AFTER_MS);
  a.set(PAUSE);
  env.add(new FakeTurn({ phase: 'streaming' }), '7:aaaaaaaa-0000-4000-8000-000000000003');
  env.advance(START_AFTER_MS);
  expect(env.started).toHaveLength(2);
  expect(env.controller.activeCount).toBe(2);
  env.advance(WAITING_LINGER_MS);
  expect(env.controller.activeCount).toBe(1);
});

it('endAll also ends every activity the system still shows, tracked or not (a wipe leaves nothing on the lock screen)', () => {
  const env = setup({ leftovers: 1 });
  env.controller.start();
  // An activity the controller no longer tracks (its turn left memory while it lingered elsewhere).
  const leftover = env.leftovers[0]!;
  leftover.ended = null;
  env.controller.endAll();
  expect((leftover.ended as { policy?: unknown } | null)?.policy).toBe('immediate');
});
