/**
 * Presentation of a native device session (ADR-0025 WP3), shared by the two
 * screens that list them: the user's own Settings › Devices and the admin
 * Users › Devices drawer. Both read the same row shape
 * (`internal/api/nativeauth/devices.go`'s `UserDevice`), so the words for a
 * platform and a revoke reason are defined once.
 */
import type { NativeDevice } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/** `platform` as the server validates it: ios, android, macos, windows, linux, other. */
export function nativePlatformLabel(platform: string): string {
  switch (platform) {
    case 'ios':
      return t('shared.nativeDevices.platform.ios', 'iOS');
    case 'android':
      return t('shared.nativeDevices.platform.android', 'Android');
    case 'macos':
      return t('shared.nativeDevices.platform.macos', 'macOS');
    case 'windows':
      return t('shared.nativeDevices.platform.windows', 'Windows');
    case 'linux':
      return t('shared.nativeDevices.platform.linux', 'Linux');
    default:
      return t('shared.nativeDevices.platform.other', 'Other');
  }
}

/** Why a device was revoked, in words (the server's `revoke_reason`). */
export function nativeRevokeReasonLabel(reason: string | null | undefined): string {
  switch (reason ?? '') {
    case 'signed_out':
      return t('shared.nativeDevices.reason.signedOut', 'Signed out on the device');
    case 'user':
      return t('shared.nativeDevices.reason.user', 'Revoked by the user');
    case 'admin':
      return t('shared.nativeDevices.reason.admin', 'Revoked by an administrator');
    case 'refresh_reuse':
    case 'code_replay':
      return t('shared.nativeDevices.reason.reuse', 'Revoked after a reused credential');
    case 'user_deactivated':
      return t('shared.nativeDevices.reason.userDeactivated', 'Account suspended');
    case 'client_disabled':
      return t('shared.nativeDevices.reason.clientDisabled', 'App disabled by an administrator');
    case 'client_removed':
      return t('shared.nativeDevices.reason.clientRemoved', 'App removed by an administrator');
    case 'expired':
      return t('shared.nativeDevices.reason.expired', 'Expired');
    default:
      return t('shared.nativeDevices.reason.unknown', 'Revoked');
  }
}

/** True while the session is live. */
export function isNativeDeviceActive(device: Pick<NativeDevice, 'revoked_at'>): boolean {
  return device.revoked_at === null || device.revoked_at === undefined;
}

/** A timestamp in the viewer's locale; an unparsable value is shown as sent. */
export function formatNativeDeviceTime(value: string): string {
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}
