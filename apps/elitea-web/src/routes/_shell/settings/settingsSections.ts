/**
 * Which rows the Settings drawer shows, as a pure function of the four facts
 * that decide it.
 *
 * Extracted from `settings-layout.tsx` so the gates can be checked without a
 * router, a project store and an auth context. They are worth checking: the
 * reference applies five filters here (`pages/settings/index.jsx:150-166`)
 * and this port had implemented two, so a personal project drew three rows
 * production does not have and omitted the one it opens on.
 *
 * The URL SLUGS are this app's route-file names, not the reference's. Where
 * they differ (`model-configuration` for AI Providers, `project-params` for
 * Project Context) `shared/ui/settings/SettingsRedirect.tsx`'s `SLUG_ALIASES`
 * makes the reference's slug resolve as well, so a link copied out of
 * production still lands on the right screen.
 */
import type { SettingsSection } from '@/shared/ui/settings/SettingsDrawer';

export interface SettingsSectionGates {
  /** The selected project is the tenant's PUBLIC project. */
  readonly isPublicProject: boolean;
  /**
   * The selected project is the caller's OWN personal project.
   *
   * `routes/-guards/personalProject.ts` is what answers this, and it is not a
   * bare `selectedProjectId === personal_project_id`: this backend resolves
   * `personal_project_id` to an ordinary shared project for any account whose
   * personal project was never provisioned. See that file.
   */
  readonly isPersonalProject: boolean;
  /** `analytics_enabled` on platform settings. */
  readonly analyticsVisible: boolean;
  /** `cost_budgets_enabled` on platform settings. */
  readonly usageVisible: boolean;
}

/** Tabs that exist only in the public project (`publicOnly` in the reference). */
const PUBLIC_ONLY_TABS = ['prompts', 'environment'] as const;

/**
 * Tabs a URL can reach but the drawer may be hiding. `settings-layout.tsx`
 * redirects away from these, because each one renders an EMPTY body rather
 * than an error when its gate is closed — every query on Service Prompts and
 * Environment is disabled unless the project is public.
 */
export function isTabHidden(tab: string | undefined, gates: SettingsSectionGates): boolean {
  if (tab === undefined) return false;
  if (PUBLIC_ONLY_TABS.includes(tab as (typeof PUBLIC_ONLY_TABS)[number])) {
    return !gates.isPublicProject;
  }
  if (tab === 'users') return gates.isPersonalProject;
  if (tab === 'project-params') return gates.isPublicProject;
  return false;
}

export function buildSettingsSections(gates: SettingsSectionGates): SettingsSection[] {
  const { isPublicProject, isPersonalProject, analyticsVisible, usageVisible } = gates;

  const projectTabs = [
    { id: 'project-general', label: 'General' },
    { id: 'model-configuration', label: 'AI Providers' },
    ...(isPublicProject ? [] : [{ id: 'project-params', label: 'Project Context' }]),
    ...(isPublicProject
      ? [
          { id: 'prompts', label: 'Service Prompts' },
          { id: 'environment', label: 'Environment' },
        ]
      : []),
    { id: 'secrets', label: 'Secrets' },
    { id: 'webhooks', label: 'Webhooks' },
    ...(isPersonalProject ? [] : [{ id: 'users', label: 'Users' }]),
    ...(analyticsVisible ? [{ id: 'analytics', label: 'Analytics' }] : []),
    ...(usageVisible ? [{ id: 'usage', label: 'Usage' }] : []),
  ];

  return [
    { section: 'PROJECT', tabs: projectTabs },
    {
      section: 'PERSONAL',
      tabs: [
        { id: 'profile', label: 'Profile' },
        { id: 'preferences', label: 'Preferences' },
        { id: 'ai-personality', label: 'AI Personality' },
        { id: 'memory', label: 'Memory' },
        { id: 'tokens', label: 'Personal Tokens' },
        { id: 'notifications', label: 'Notifications' },
      ],
    },
  ];
}
