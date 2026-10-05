import { checkVersionAgain } from '@/components/UpgradeRequired';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

// DEF-R4: "Check again" on the update screen re-reads the workspace's minimum.
describe('checkVersionAgain', () => {
  beforeEach(() => __resetSessionForTests());

  it('is true once the re-read cleared the update screen', async () => {
    setSession({ upgradeRequired: { minVersion: '9.0.0' } });
    const refresh = jest.fn(async () => setSession({ upgradeRequired: null }));
    expect(await checkVersionAgain(refresh)).toBe(true);
    expect(refresh).toHaveBeenCalledTimes(1);
  });

  it('is false while the workspace still needs a newer version', async () => {
    setSession({ upgradeRequired: { minVersion: '9.0.0' } });
    expect(await checkVersionAgain(async () => undefined)).toBe(false);
  });
});
