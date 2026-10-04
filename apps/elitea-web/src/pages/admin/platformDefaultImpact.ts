/**
 * The sentence a platform-model delete confirmation adds when the model is a
 * stored default (#6826). The server clears those defaults after the delete;
 * this tells the operator who falls back before they confirm.
 *
 * The i18n shim has no plural forms, so 1 and N are separate keys, and a count
 * of 0 is left out. "Use it" and not "chose it": a new project is seeded with
 * the platform default, so most counted projects chose nothing themselves.
 */
import { t } from '@/shared/i18n';

import type { PlatformDefaultUsage } from './api/adminPlatformDefaultModelApi';

/** undefined when the model is nobody's default, or the count is not known yet. */
export function platformDefaultImpact(usage: PlatformDefaultUsage | undefined): string | undefined {
  if (usage === undefined || usage.served_by_another_row) return undefined;
  if (usage.platform_default) return platformImpact(usage.projects);
  if (usage.projects === 1) {
    return t(
      'pages.admin.platformDefault.deleteImpactProjectsOne',
      '1 project uses it as its default. It moves to the platform default.',
    );
  }
  if (usage.projects > 1) {
    return t(
      'pages.admin.platformDefault.deleteImpactProjectsMany',
      '{{count}} projects use it as their default. They move to the platform default.',
      { count: usage.projects },
    );
  }
  return undefined;
}

function platformImpact(projects: number): string {
  if (projects === 1) {
    return t(
      'pages.admin.platformDefault.deleteImpactPlatformOne',
      'It is the platform default and the own default of 1 project. It, and projects with no own default, move to the first available model.',
    );
  }
  if (projects > 1) {
    return t(
      'pages.admin.platformDefault.deleteImpactPlatformMany',
      'It is the platform default and the own default of {{count}} projects. They, and projects with no own default, move to the first available model.',
      { count: projects },
    );
  }
  return t(
    'pages.admin.platformDefault.deleteImpactPlatform',
    'It is the platform default. Projects with no own default move to the first available model.',
  );
}

/** The line shown when the count could not be read. */
export function platformDefaultImpactUnknown(): string {
  return t(
    'pages.admin.platformDefault.deleteImpactUnknown',
    'Could not count the projects that use this model as their default.',
  );
}
