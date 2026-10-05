import { prepareTurn } from '@/api/queries/sendMessage';
import { migrate } from '@/db/migrations';
import { offlineStore } from '@/db/offlineStore';
import { Db } from '@/db/sql';
import { __setOnlineForTests, isOnlineState } from '@/lib/network';
import { bindOutbox, handOffToOutbox, outboxDrainer, queueSend, shouldQueue } from '@/sync/appOutbox';
import { listOutbox, outboxStatus } from '@/sync/outbox';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';

afterEach(() => {
  __setOnlineForTests(true);
  jest.restoreAllMocks();
});

describe('isOnlineState', () => {
  it('counts only a definite "not connected" or "not reachable" as offline', () => {
    expect(isOnlineState({ isConnected: true, isInternetReachable: true })).toBe(true);
    expect(isOnlineState({})).toBe(true);
    expect(isOnlineState({ isConnected: false })).toBe(false);
    expect(isOnlineState({ isConnected: true, isInternetReachable: false })).toBe(false);
  });
});

describe('offline send', () => {
  it('queues only when offline with the store open', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db);
    const get = jest.spyOn(offlineStore, 'get').mockReturnValue(null);
    __setOnlineForTests(false);
    expect(shouldQueue()).toBe(false); // offline storage off: nothing is kept on the device
    get.mockReturnValue(db);
    expect(shouldQueue()).toBe(true);
    __setOnlineForTests(true);
    expect(shouldQueue()).toBe(false);

    const listener = jest.fn();
    const unsubscribe = outboxDrainer.subscribe(listener);
    const prepared = prepareTurn({
      projectId: 7,
      conversationUuid: UUID,
      participantId: 2,
      questionId: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f',
      userInput: 'written offline',
    });
    await queueSend(prepared, '42');
    unsubscribe();
    expect(listener).toHaveBeenCalled();
    expect(await listOutbox(db, 7, UUID)).toEqual([expect.objectContaining({ bodyJson: prepared.bodyJson, state: 'queued' })]);
  });
});

// DEF-5: a live send the workspace did not answer while the device is online.
describe('hand-off of an unanswered live send', () => {
  const prepared = () =>
    prepareTurn({
      projectId: 7,
      conversationUuid: UUID,
      participantId: 2,
      questionId: '1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f',
      userInput: 'sent while the workspace hung',
    });

  it('queues it as already tried once, in backoff, and says the workspace is unreachable', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db);
    jest.spyOn(offlineStore, 'get').mockReturnValue(db);
    const drain = jest.spyOn(outboxDrainer, 'drain').mockResolvedValue({ sent: 0, failed: 0, waiting: 1 });
    const before = Date.now();
    expect(await handOffToOutbox(prepared(), '42')).toBe(true);
    const [item] = await listOutbox(db, 7, UUID);
    expect(item).toMatchObject({ bodyJson: prepared().bodyJson, state: 'queued', attempts: 1, conversationRef: '42' });
    expect(item.nextAttemptMs).toBeGreaterThan(before);
    expect(outboxStatus(item).text).toBe('Queued. Retrying when the workspace can be reached.');
    // The drain schedules the wake for the end of the backoff.
    expect(drain).toHaveBeenCalled();
  });

  // DEF-R3: a proxy's 503 with Retry-After: the outbox waits at least that long.
  it("waits for the server's Retry-After before the first outbox attempt", async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db);
    jest.spyOn(offlineStore, 'get').mockReturnValue(db);
    jest.spyOn(outboxDrainer, 'drain').mockResolvedValue({ sent: 0, failed: 0, waiting: 1 });
    const before = Date.now();
    expect(await handOffToOutbox(prepared(), '42', { retryAfterMs: 45_000 })).toBe(true);
    const [item] = await listOutbox(db, 7, UUID);
    expect(item.nextAttemptMs).toBeGreaterThanOrEqual(before + 45_000);
  });

  it('declines when offline storage is off', async () => {
    jest.spyOn(offlineStore, 'get').mockReturnValue(null);
    expect(await handOffToOutbox(prepared(), '42')).toBe(false);
  });
});

describe('bindOutbox', () => {
  it('drains on reconnect (and syncs), when the store opens and on foreground', () => {
    let online: ((v: boolean) => void) | null = null;
    let status: 'open' | 'closed' = 'closed';
    let storeListener: (() => void) | null = null;
    let activity: ((s: 'active' | 'background') => void) | null = null;
    const drain = jest.fn();
    const onReconnect = jest.fn();
    bindOutbox({
      store: {
        get status() {
          return status;
        },
        subscribe: (l) => {
          storeListener = l;
          return () => {};
        },
      },
      network: {
        subscribe: (l) => {
          online = l;
          return () => {};
        },
      },
      activity: {
        addEventListener: (_t, l) => {
          activity = l as typeof activity;
          return { remove: () => {} };
        },
      },
      drain,
      onReconnect,
    });
    expect(drain).not.toHaveBeenCalled();
    online!(false);
    expect(drain).not.toHaveBeenCalled();
    online!(true);
    expect(drain).toHaveBeenCalledTimes(1);
    expect(onReconnect).toHaveBeenCalledTimes(1);
    status = 'open';
    storeListener!();
    expect(drain).toHaveBeenCalledTimes(2);
    activity!('active');
    expect(drain).toHaveBeenCalledTimes(3);
  });

  // F1: a hold schedules no wake, so the drain resumes when the update screen clears.
  it('drains when the update screen clears (upgradeRequired set → null)', () => {
    let upgradeListener: (() => void) | null = null;
    let blocked = true;
    const drain = jest.fn();
    bindOutbox({
      store: { status: 'closed', subscribe: () => () => {} },
      network: { subscribe: () => () => {} },
      activity: { addEventListener: () => ({ remove: () => {} }) },
      upgrade: {
        blocked: () => blocked,
        subscribe: (l) => {
          upgradeListener = l;
          return () => {};
        },
      },
      drain,
    });
    upgradeListener!();
    expect(drain).not.toHaveBeenCalled(); // still blocked
    blocked = false;
    upgradeListener!();
    expect(drain).toHaveBeenCalledTimes(1);
    upgradeListener!();
    expect(drain).toHaveBeenCalledTimes(1); // only on the transition
  });
});
