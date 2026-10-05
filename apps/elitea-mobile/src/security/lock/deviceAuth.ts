import * as LocalAuthentication from 'expo-local-authentication';
import { Platform } from 'react-native';

/**
 * The only use of `expo-local-authentication`. Biometrics with the device
 * passcode as fallback ("biometric or passcode").
 */

/** What the device can use to prove its owner is present. */
export type DeviceSecurity = 'none' | 'passcode' | 'biometric' | 'unsupported';

export type AuthOutcome = { ok: true } | { ok: false; reason: 'cancelled' | 'lockout' | 'not_available' | 'failed' };

export interface DeviceAuth {
  security(): Promise<DeviceSecurity>;
  authenticate(promptMessage: string): Promise<AuthOutcome>;
}

export const deviceAuth: DeviceAuth = {
  async security() {
    if (Platform.OS === 'web') return 'unsupported';
    const level = await LocalAuthentication.getEnrolledLevelAsync();
    switch (level) {
      case LocalAuthentication.SecurityLevel.NONE:
        return 'none';
      case LocalAuthentication.SecurityLevel.SECRET:
        return 'passcode';
      default:
        return 'biometric';
    }
  },

  async authenticate(promptMessage) {
    try {
      const result = await LocalAuthentication.authenticateAsync({
        promptMessage,
        cancelLabel: 'Cancel',
        disableDeviceFallback: false,
      });
      if (result.success) return { ok: true };
      switch (result.error) {
        case 'user_cancel':
        case 'system_cancel':
        case 'app_cancel':
          return { ok: false, reason: 'cancelled' };
        case 'lockout':
          return { ok: false, reason: 'lockout' };
        case 'not_enrolled':
        case 'not_available':
        case 'passcode_not_set':
          return { ok: false, reason: 'not_available' };
        default:
          return { ok: false, reason: 'failed' };
      }
    } catch {
      return { ok: false, reason: 'failed' };
    }
  },
};
