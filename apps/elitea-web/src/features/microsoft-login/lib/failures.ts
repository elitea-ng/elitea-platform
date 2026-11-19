/**
 * Why a sign-in did not finish, in words a person can act on.
 *
 * A code is one of: the client's own (`popup_blocked`, `save_failed`, `cancelled`,
 * `expired`), a refusal of start or redeem (`custom_apps_disabled`, `handoff_invalid`, ...),
 * or an `error` the callback page reported (`denied`, `tenant_mismatch`, ...). Anything
 * else reads as the generic failure. No text from Microsoft is ever shown.
 */
import { t } from '@/shared/i18n';

const MESSAGES: Readonly<Record<string, () => string>> = {
  popup_blocked: () => t('features.microsoftLogin.failure.popupBlocked', 'The sign-in window was blocked. Allow pop-ups for this site, then try again.'),
  save_failed: () => t('features.microsoftLogin.failure.saveFailed', 'The credential could not be saved, so the sign-in did not start. Fix the fields marked in the form and try again.'),
  cancelled: () => t('features.microsoftLogin.failure.cancelled', 'The sign-in was cancelled.'),
  expired: () => t('features.microsoftLogin.failure.expired', 'The sign-in took too long and expired. Sign in again.'),
  denied: () => t('features.microsoftLogin.failure.denied', 'The sign-in was cancelled in the Microsoft window.'),
  custom_apps_disabled: () => t('features.microsoftLogin.failure.customAppsDisabled', 'An administrator turned off Custom app sign-in. Choose a Default app for this credential.'),
  default_apps_unavailable: () => t('features.microsoftLogin.failure.defaultAppsUnavailable', 'The Default app this credential uses is not available. Choose another app, or ask an administrator.'),
  too_many_sign_ins: () => t('features.microsoftLogin.failure.tooManySignIns', 'Too many sign-ins are open. Finish or close them, then try again in a few minutes.'),
  tenant_mismatch: () => t('features.microsoftLogin.failure.tenantMismatch', 'This account belongs to a different Microsoft tenant than the one the app is set up for.'),
  permission_missing: () => t('features.microsoftLogin.failure.permissionMissing', 'The account lacks a permission this credential needs. Tick it in the credential and sign in again.'),
  handoff_invalid: () => t('features.microsoftLogin.failure.handoffInvalid', 'The sign-in is no longer valid. Sign in again.'),
  credential_changed: () => t('features.microsoftLogin.failure.credentialChanged', 'The credential changed while you were signing in. Sign in again.'),
};

export function failureMessage(code: string): string {
  const message = Object.hasOwn(MESSAGES, code) ? MESSAGES[code] : undefined;
  return message === undefined ? t('features.microsoftLogin.failure.generic', 'The Microsoft sign-in did not finish. Try again.') : message();
}
