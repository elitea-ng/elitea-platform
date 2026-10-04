/**
 * The sentence a platform-model delete confirmation adds when the model is a
 * stored default (#6826). The server clears those defaults after the delete;
 * this tells the operator who falls back before they confirm.
 */
import { t } from '@/shared/i18n';

import type { PlatformDefaultUsage } from './api/adminPlatformDefaultModelApi';

/** undefined when the model is nobody's default, or the count is not known yet. */
export function platformDefaultImpact(usage: PlatformDefaultUsage | undefined): string | undefined {
  if (usage === undefined || (!usage.platform_default && usage.projects === 0)) return undefined;
  if (usage.platform_default) {
    return t(
      'pages.admin.platformDefault.deleteImpactPlatform',
      'It is the platform default, and {{count}} projects chose it as their own. They fall back to the first available model until you choose a new default.',
      { count: usage.projects },
    );
  }
  return t(
    'pages.admin.platformDefault.deleteImpactProjects',
    '{{count}} projects chose it as their default. They fall back to the platform default.',
    { count: usage.projects },
  );
}
