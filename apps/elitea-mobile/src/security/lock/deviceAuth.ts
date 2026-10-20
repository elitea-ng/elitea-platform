import * as LocalAuthentication from 'expo-local-authentication';
import { Platform } from 'react-native';

/**
 * The only use of `expo-local-authentication`. Biometrics with the device
 * passcode as fallback ("biometric or passcode").
 */

/** What the device can use to prove its owner is present. */
export type DeviceSecurity = 'none' | 'passcode' | 'biometric' | 'unsupported';

export type AuthOutcome = { ok: true } | { ok: false; reason: 'cancelled' | 'lockout' | 'not_available' | 'failed' };

/** The biometric the device unlocks with, for the unlock button's words and glyph. */
export type Biometry = 'face' | 'fingerprint' | 'iris' | null;

export interface DeviceAuth {
  security(): Promise<DeviceSecurity>;
  authenticate(promptMessage: string): Promise<AuthOutcome>;
  /** Which biometric is enrolled-capable (Face ID, Touch ID / fingerprint, iris); null: none. */
  biometry?(): Promise<Biometry>;
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

  async biometry() {
    if (Platform.OS === 'web') return null;
    try {
      const types = await LocalAuthentication.supportedAuthenticationTypesAsync();
      const T = LocalAuthentication.AuthenticationType;
      if (types.includes(T.FACIAL_RECOGNITION)) return 'face';
      if (types.includes(T.FINGERPRINT)) return 'fingerprint';
      if (types.includes(T.IRIS)) return 'iris';
      return null;
    } catch {
      return null;
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
