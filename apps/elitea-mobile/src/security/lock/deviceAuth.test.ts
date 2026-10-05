import * as LocalAuthentication from 'expo-local-authentication';

import { deviceAuth } from '@/security/lock/deviceAuth';

jest.mock('expo-local-authentication', () => ({
  SecurityLevel: { NONE: 0, SECRET: 1, BIOMETRIC_WEAK: 2, BIOMETRIC_STRONG: 3 },
  getEnrolledLevelAsync: jest.fn(),
  authenticateAsync: jest.fn(),
}));

const mocked = LocalAuthentication as jest.Mocked<typeof LocalAuthentication>;

describe('deviceAuth', () => {
  it.each([
    [0, 'none'],
    [1, 'passcode'],
    [2, 'biometric'],
    [3, 'biometric'],
  ])('maps enrolled level %s to %s', async (level, expected) => {
    mocked.getEnrolledLevelAsync.mockResolvedValueOnce(level);
    await expect(deviceAuth.security()).resolves.toBe(expected);
  });

  it('allows the passcode fallback', async () => {
    mocked.authenticateAsync.mockResolvedValueOnce({ success: true });
    await expect(deviceAuth.authenticate('Unlock')).resolves.toEqual({ ok: true });
    expect(mocked.authenticateAsync).toHaveBeenCalledWith(
      expect.objectContaining({ promptMessage: 'Unlock', disableDeviceFallback: false }),
    );
  });

  it.each([
    ['user_cancel', 'cancelled'],
    ['system_cancel', 'cancelled'],
    ['lockout', 'lockout'],
    ['passcode_not_set', 'not_available'],
    ['authentication_failed', 'failed'],
  ] as const)('maps %s to %s', async (error, reason) => {
    mocked.authenticateAsync.mockResolvedValueOnce({ success: false, error });
    await expect(deviceAuth.authenticate('Unlock')).resolves.toEqual({ ok: false, reason });
  });

  it('reports a throwing prompt as failed', async () => {
    mocked.authenticateAsync.mockRejectedValueOnce(new Error('boom'));
    await expect(deviceAuth.authenticate('Unlock')).resolves.toEqual({ ok: false, reason: 'failed' });
  });
});
