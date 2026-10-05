import { queryClient } from '@/api/queryClient';
import { inFlightCount, projectScope, registerAbortable, abortAll } from '@/lib/abortRegistry';
import { SECURE_KEYS } from '@/security/secureStorage';
import { selectProject } from '@/state/projectSelection';
import { __resetSessionForTests, getSession, setSession } from '@/state/session';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

beforeEach(() => {
  __resetSessionForTests();
  abortAll();
  queryClient.clear();
  fake.values.clear();
});

describe('selectProject', () => {
  it('stops the old project streams, drops its cache and persists the choice', async () => {
    setSession({ projectId: 1 });
    const oldStream = new AbortController();
    const otherWork = new AbortController();
    registerAbortable(oldStream, projectScope(1));
    registerAbortable(otherWork);
    queryClient.setQueryData(['p', 1, 'conversations'], 'old');
    queryClient.setQueryData(['projects'], 'list');

    await selectProject(2);

    expect(getSession().projectId).toBe(2);
    expect(oldStream.signal.aborted).toBe(true);
    expect(otherWork.signal.aborted).toBe(false);
    expect(inFlightCount()).toBe(1);
    expect(queryClient.getQueryData(['p', 1, 'conversations'])).toBeUndefined();
    expect(queryClient.getQueryData(['projects'])).toBe('list');
    expect(fake.values.get(SECURE_KEYS.projectSelected)).toBe('2');
  });

  it('is a no-op for the current project and rejects invalid ids', async () => {
    setSession({ projectId: 3 });
    queryClient.setQueryData(['p', 3, 'conversations'], 'keep');
    await selectProject(3);
    expect(queryClient.getQueryData(['p', 3, 'conversations'])).toBe('keep');
    await expect(selectProject(0)).rejects.toThrow();
  });
});

afterAll(() => queryClient.clear());
