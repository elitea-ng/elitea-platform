import { LOCKED_VERSION_STATUSES } from './toolPicker.constants';

/**
 * Published and embedded versions cannot change their attachments (the backend rejects it).
 * Accepts the version status explicitly (no ambient Formik context in `apps/elitea-web`).
 */
export function isVersionLocked(status: string | undefined): boolean {
  return Boolean(status && LOCKED_VERSION_STATUSES.includes(status));
}

export function useIsVersionLocked(status: string | undefined): boolean {
  return isVersionLocked(status);
}
