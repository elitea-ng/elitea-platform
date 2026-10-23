/**
 * The words for a failed read or write of a project's Send to cloud switch.
 * The routes answer 409 when "On" is asked for while the platform switch is
 * off, 403 without the permission, and 404 for a project that is gone.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

export function describeCloudExecutorSwitchError(error: unknown): string {
  const status = error instanceof EliteaApiError && 'status' in error.failure ? error.failure.status : 0;
  switch (status) {
    case 409:
      return t(
        'features.cloudExecutorSwitch.error.platformOff',
        'The cloud executor is off for this deployment, so it cannot be turned on for a project.',
      );
    case 403:
      return t('features.cloudExecutorSwitch.error.forbidden', 'You do not have permission to change this setting.');
    case 404:
      return t('features.cloudExecutorSwitch.error.notFound', 'This project no longer exists.');
    default:
      return t('features.cloudExecutorSwitch.error.generic', 'Could not read or save the Send to cloud setting.');
  }
}
