import { describe, expect, it } from 'vitest';

import {
  formatNativeDeviceTime,
  isNativeDeviceActive,
  nativePlatformLabel,
  nativeRevokeReasonLabel,
} from './nativeDevices';

describe('nativeDevices presentation', () => {
  it('names every platform the server validates, and folds anything else into Other', () => {
    expect(['ios', 'android', 'macos', 'windows', 'linux', 'other', 'beos'].map(nativePlatformLabel)).toEqual([
      'iOS',
      'Android',
      'macOS',
      'Windows',
      'Linux',
      'Other',
      'Other',
    ]);
  });

  it('words every revoke reason the server sends, and never renders a raw code', () => {
    const reasons = [
      'signed_out',
      'user',
      'admin',
      'refresh_reuse',
      'code_replay',
      'user_deactivated',
      'client_disabled',
      'client_removed',
      'expired',
    ];
    for (const reason of reasons) {
      expect(nativeRevokeReasonLabel(reason), reason).not.toContain('_');
      expect(nativeRevokeReasonLabel(reason), reason).not.toBe('Revoked');
    }
    expect(nativeRevokeReasonLabel(null)).toBe('Revoked');
  });

  it('treats a device with no revoked_at as active', () => {
    expect(isNativeDeviceActive({ revoked_at: null })).toBe(true);
    expect(isNativeDeviceActive({ revoked_at: '2026-10-01T00:00:00Z' })).toBe(false);
  });

  it('shows an unparsable timestamp as sent', () => {
    expect(formatNativeDeviceTime('not a date')).toBe('not a date');
  });
});
