import { QueryObserver, type QueryObserverResult } from '@tanstack/react-query';

import { ApiError } from '@/api/errors';
import { queryClient, retryDelay, shouldRetry, wireUpgradeRecovery } from '@/api/queryClient';
import { endSession } from '@/security/endSession';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

describe('shouldRetry', () => {
  it('retries transient failures a bounded number of times', () => {
    expect(shouldRetry(0, new ApiError('network', 'x'))).toBe(true);
    expect(shouldRetry(1, new ApiError('timeout', 'x'))).toBe(true);
    expect(shouldRetry(0, new ApiError('http', 'x', { status: 503 }))).toBe(true);
    expect(shouldRetry(0, new ApiError('http', 'x', { status: 429 }))).toBe(true);
    expect(shouldRetry(2, new ApiError('network', 'x'))).toBe(false);
  });

  it('never retries a 4xx, auth, upgrade, origin or cancellation failure', () => {
    for (const status of [400, 403, 404, 409, 422]) {
      expect(shouldRetry(0, new ApiError('http', 'x', { status }))).toBe(false);
    }
    for (const kind of ['session_invalid', 'upgrade_required', 'origin_mismatch', 'aborted', 'session_ending'] as const) {
      expect(shouldRetry(0, new ApiError(kind, 'x'))).toBe(false);
    }
    expect(shouldRetry(0, new Error('plain'))).toBe(false);
  });

  it('honours Retry-After and caps the exponential delay', () => {
    expect(retryDelay(0, new ApiError('http', 'x', { status: 503, retryAfterMs: 2000 }))).toBe(2000);
    expect(retryDelay(0, new Error('x'))).toBe(1000);
    expect(retryDelay(10, new Error('x'))).toBe(15_000);
  });
});

describe('query cache wipe', () => {
  beforeEach(() => __resetSessionForTests());

  it('endSession clears the query cache', async () => {
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    setSession({ hydrated: true, serverOrigin: 'https://ai.example.com' });
    queryClient.setQueryData(['p', 1, 'conversations'], { rows: [1] });
    await endSession({ reason: 'user_logout' });
    expect(queryClient.getQueryData(['p', 1, 'conversations'])).toBeUndefined();
    jest.restoreAllMocks();
  });
});

afterAll(() => queryClient.clear());

describe('when the update screen clears (DEF-R4)', () => {
  beforeEach(() => {
    __resetSessionForTests();
    queryClient.clear();
  });

  async function settle(observer: QueryObserver<string>, ok: (r: QueryObserverResult<string>) => boolean) {
    for (let i = 0; i < 50 && !ok(observer.getCurrentResult()); i++) {
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    return observer.getCurrentResult();
  }

  it('refetches the queries a 426 failed, so the screen underneath is not left on an error', async () => {
    const stop = wireUpgradeRecovery();
    setSession({ hydrated: true, serverOrigin: 'https://ai.example.com', upgradeRequired: { minVersion: '9.0.0' } });
    let calls = 0;
    const observer = new QueryObserver<string>(queryClient, {
      queryKey: ['p', 1, 'conversation', 'c1'],
      queryFn: async () => {
        calls += 1;
        if (calls === 1) throw new ApiError('upgrade_required', 'too old', { status: 426 });
        return 'transcript';
      },
    });
    const unsubscribe = observer.subscribe(() => {});
    expect((await settle(observer, (r) => r.status === 'error')).status).toBe('error');

    setSession({ upgradeRequired: null });

    const result = await settle(observer, (r) => r.data === 'transcript');
    expect(result.data).toBe('transcript');
    expect(calls).toBe(2);
    unsubscribe();
    stop();
  });

  it('leaves a query that failed for another reason alone', async () => {
    const stop = wireUpgradeRecovery();
    setSession({ hydrated: true, serverOrigin: 'https://ai.example.com', upgradeRequired: { minVersion: '9.0.0' } });
    const queryFn = jest.fn(async (): Promise<string> => {
      throw new ApiError('http', 'missing', { status: 404 });
    });
    const observer = new QueryObserver<string>(queryClient, { queryKey: ['p', 1, 'conversation', 'gone'], queryFn });
    const unsubscribe = observer.subscribe(() => {});
    await settle(observer, (r) => r.status === 'error');

    setSession({ upgradeRequired: null });
    await new Promise((resolve) => setTimeout(resolve, 30));

    expect(queryFn).toHaveBeenCalledTimes(1);
    unsubscribe();
    stop();
  });
});
