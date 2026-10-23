import { endSession, registerRevokeHook } from '@/security/endSession';
import { bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests, setSession } from '@/state/session';

/**
 * The app's Live Activity wiring (turnActivities.ts) against the sign-out
 * order: the lock screen is state the OS keeps outside the app, so it is
 * cleared BEFORE the capped server revoke (QA-SEC-1, NS-1 / NS-SEC-2), as the
 * widget snapshot and the Siri catalog are.
 */

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

type Ended = { policy: unknown };
interface FakeActivity {
  props: Record<string, unknown>;
  ends: Ended[];
}

const mockStarted: FakeActivity[] = [];
const mockLeftovers: FakeActivity[] = [];

function mockHandle(fake: FakeActivity) {
  return {
    update: async (props: Record<string, unknown>) => {
      fake.props = props;
    },
    end: async (policy?: unknown) => {
      fake.ends.push({ policy });
    },
  };
}

jest.mock('@/features/surfaces/TurnActivity', () => ({
  turnActivity: {
    start: (props: Record<string, unknown>) => {
      const fake: FakeActivity = { props, ends: [] };
      mockStarted.push(fake);
      return mockHandle(fake);
    },
    getInstances: () => mockLeftovers.map(mockHandle),
  },
}));

type Listener = () => void;
const mockLiveTurns = new Map<string, { getSnapshot: () => unknown; subscribe: (l: Listener) => () => void }>();
const mockRegistryListeners = new Set<Listener>();

jest.mock('@/features/chat/turnController', () => ({
  listLiveTurns: () => [...mockLiveTurns.entries()],
  subscribeLiveTurns: (l: Listener) => {
    mockRegistryListeners.add(l);
    return () => mockRegistryListeners.delete(l);
  },
}));

function fakeTurn(phase: string, extra: Record<string, unknown> = {}) {
  const { INITIAL_TURN } = jest.requireActual<typeof import('@/features/chat/turnReducer')>('@/features/chat/turnReducer');
  let state = { questionId: 'q', userText: 'secret', turn: { ...INITIAL_TURN, phase, ...extra }, queued: false };
  const listeners = new Set<Listener>();
  return {
    getSnapshot: () => state,
    subscribe: (l: Listener) => (listeners.add(l), () => listeners.delete(l)),
    set(turn: Record<string, unknown>) {
      state = { ...state, turn: { ...state.turn, ...turn } };
      for (const l of [...listeners]) l();
    },
  };
}

function publishRegistry() {
  for (const l of [...mockRegistryListeners]) l();
}

beforeEach(async () => {
  jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
  mockStarted.length = 0;
  mockLeftovers.length = 0;
  mockLiveTurns.clear();
  __resetSessionForTests();
  await bindWorkspace('aaaaaaaaaaaa');
  setSession({ workspaceId: 'aaaaaaaaaaaa' });
});

afterEach(() => {
  jest.useRealTimers();
});

it('ends every Live Activity (lingering, running and leftover) BEFORE the server revoke of a log out', async () => {
  const { startTurnActivities } = require('@/features/surfaces/turnActivities') as typeof import('@/features/surfaces/turnActivities');
  startTurnActivities();

  // One answer paused for approval (its activity lingers 15 min, retired once the thread drops it)...
  const paused = fakeTurn('streaming');
  mockLiveTurns.set('7:0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e', paused);
  publishRegistry();
  // ...and one still running.
  const running = fakeTurn('streaming');
  mockLiveTurns.set('7:1c9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e', running);
  publishRegistry();
  jest.advanceTimersByTime(5_000);
  expect(mockStarted).toHaveLength(2);

  paused.set({ phase: 'paused', pause: { kind: 'hitl', shape: 'single', interrupts: [], threadId: null, messageId: 'm' } });
  mockLiveTurns.delete('7:0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e');
  publishRegistry();
  expect(mockStarted[0]!.ends.map((e) => e.policy)).toEqual([expect.objectContaining({ after: expect.any(Date) })]);

  // And one left on the lock screen by a killed process.
  mockLeftovers.push({ props: {}, ends: [] });

  let atRevoke: { lingering: unknown[]; running: unknown[]; leftover: unknown[] } | undefined;
  const off = registerRevokeHook(async () => {
    atRevoke = {
      lingering: mockStarted[0]!.ends.map((e) => e.policy),
      running: mockStarted[1]!.ends.map((e) => e.policy),
      leftover: mockLeftovers[0]!.ends.map((e) => e.policy),
    };
  });
  jest.useRealTimers();
  try {
    await endSession({ reason: 'user_logout' });
  } finally {
    off();
  }
  expect(atRevoke).toBeDefined();
  expect(atRevoke!.lingering).toContain('immediate');
  expect(atRevoke!.running).toContain('immediate');
  expect(atRevoke!.leftover).toContain('immediate');
});
