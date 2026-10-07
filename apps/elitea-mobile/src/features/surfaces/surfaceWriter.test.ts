import { endSession, registerRevokeHook, registerWipeHook, suspendActiveWorkspace } from '@/security/endSession';
import { bindWorkspace } from '@/security/secureStorage';
import { buildSnapshot } from '@/features/surfaces/surfaceSnapshot';
import {
  RUNNING_STALE_MS,
  __setWidgetSinkForTests,
  clearSurfaceSnapshot,
  clearSurfaceSnapshotAtLaunch,
  publishSurfaceSnapshot,
  type WidgetSink,
} from '@/features/surfaces/surfaceWriter';
import { __resetSessionForTests } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const NOW = Date.UTC(2026, 9, 6, 12);
const snapshot = (over: { unread?: number; running?: number } = {}) =>
  buildSnapshot({ workspaceId: 'aaaaaaaaaaaa', projectId: 7, origin: 'https://ai.example.com', allowTitles: false, approvals: [], unread: over.unread ?? 2, running: over.running ?? 0, now: NOW });

let sink: { updateTimeline: jest.Mock };

beforeEach(() => {
  __resetSessionForTests();
  bindWorkspace('aaaaaaaaaaaa');
  sink = { updateTimeline: jest.fn() };
  __setWidgetSinkForTests(sink as WidgetSink);
});

it('writes the snapshot as the current timeline entry, and skips an unchanged one', () => {
  publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot());
  publishSurfaceSnapshot('aaaaaaaaaaaa', { ...snapshot(), at: NOW + 1000 });
  expect(sink.updateTimeline).toHaveBeenCalledTimes(1);
  const [entries] = sink.updateTimeline.mock.calls[0];
  expect(entries).toHaveLength(1);
  expect(entries[0].props.unread).toBe(2);
});

it('a running answer is dropped from the widget RUNNING_STALE_MS later (the app cannot correct it once suspended)', () => {
  publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot({ running: 2 }));
  const [entries] = sink.updateTimeline.mock.calls[0];
  expect(entries.map((e: { date: Date }) => e.date.getTime())).toEqual([NOW, NOW + RUNNING_STALE_MS]);
  expect(entries[1].props).toMatchObject({ running: 0, unread: 2, signedIn: true });
});

it('log out and a workspace switch leave only the signed-out snapshot', async () => {
  publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot());
  await endSession({ reason: 'user_logout' });
  expect(sink.updateTimeline.mock.lastCall[0][0].props).toMatchObject({ signedIn: false, unread: 0, titles: [] });

  publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot({ unread: 5 }));
  await suspendActiveWorkspace();
  expect(sink.updateTimeline.mock.lastCall[0][0].props.signedIn).toBe(false);
});

it('a snapshot republished by a re-render while the session ends does not outlive the wipe', async () => {
  // A later wipe hook (live turns cleared, the store closed) re-renders the signed-in layout,
  // whose effect still sees the old workspace: its write must not land after the clear.
  bindWorkspace('aaaaaaaaaaaa');
  publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot());
  const off = registerWipeHook('zz_rerender', () => publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot({ unread: 7 })));
  try {
    await endSession({ reason: 'user_logout' });
    expect(sink.updateTimeline.mock.lastCall[0][0].props.signedIn).toBe(false);

    bindWorkspace('aaaaaaaaaaaa');
    publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot({ unread: 3 }));
    await suspendActiveWorkspace();
    expect(sink.updateTimeline.mock.lastCall[0][0].props.signedIn).toBe(false);
  } finally {
    off();
  }
});

it('writes nothing for a workspace that is not the bound one', () => {
  bindWorkspace('bbbbbbbbbbbb');
  publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot());
  expect(sink.updateTimeline).not.toHaveBeenCalled();
  bindWorkspace(null);
});

it('never throws when the App Group write fails', () => {
  sink.updateTimeline.mockImplementation(() => {
    throw new Error('no app group');
  });
  expect(() => publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot())).not.toThrow();
  expect(() => clearSurfaceSnapshot()).not.toThrow();
});

it('does nothing off iOS (no sink)', () => {
  __setWidgetSinkForTests(null);
  expect(() => publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot())).not.toThrow();
});

it('clears the widget snapshot BEFORE the server revoke of a log out (a kill during it leaves no counts behind)', async () => {
  // The App Group outlives the process, and a wipe resumed at launch runs no in-memory hook.
  publishSurfaceSnapshot('aaaaaaaaaaaa', snapshot({ unread: 4 }));
  let atRevoke: unknown = null;
  const off = registerRevokeHook(async () => {
    atRevoke = sink.updateTimeline.mock.lastCall[0][0].props.signedIn;
  });
  try {
    await endSession({ reason: 'user_logout' });
  } finally {
    off();
  }
  expect(atRevoke).toBe(false);
});

it('clearSurfaceSnapshotAtLaunch rewrites the signed-out snapshot even when this process wrote nothing', () => {
  // What a previous process left in the App Group is unknown to this one.
  clearSurfaceSnapshotAtLaunch();
  clearSurfaceSnapshotAtLaunch();
  expect(sink.updateTimeline).toHaveBeenCalledTimes(2);
  expect(sink.updateTimeline.mock.lastCall[0][0].props).toMatchObject({ signedIn: false, unread: 0, titles: [], host: '' });
});
