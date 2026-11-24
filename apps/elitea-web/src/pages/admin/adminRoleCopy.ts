/**
 * What the console calls each built-in administration role, and its one-line
 * description (ADR-0059 decision 3, owner decision 7).
 *
 * The copy is keyed by the role's name, but it is used only for a role the
 * server reports as built-in (a seed row, `./api/adminRoleOptionsApi`): a
 * custom role is shown by its own name, whatever that name is. Which roles
 * exist, and which are offered, is never decided here.
 */
import { t } from '@/shared/i18n';

import type { AdministrationRole } from './api/adminRoleOptionsApi';

interface RoleCopy {
  readonly label: () => string;
  readonly description: () => string;
}

const BUILT_IN_ROLE_COPY: Readonly<Record<string, RoleCopy>> = {
  super_admin: {
    label: () => t('pages.admin.users.roles.superAdmin.label', 'Global administrator'),
    description: () => t('pages.admin.users.roles.superAdmin.description', 'The owner of the deployment.'),
  },
  admin: {
    label: () => t('pages.admin.users.roles.admin.label', 'Platform administrator'),
    description: () => t('pages.admin.users.roles.admin.description', 'Day-to-day platform administration.'),
  },
  llm_manager: {
    label: () => t('pages.admin.users.roles.llmManager.label', 'LLM manager'),
    description: () =>
      t(
        'pages.admin.users.roles.llmManager.description',
        'Providers, models, the catalogue, routing, budgets and alerts.',
      ),
  },
  project_administrator: {
    label: () => t('pages.admin.users.roles.projectAdministrator.label', 'Project administrator (all projects)'),
    description: () =>
      t(
        'pages.admin.users.roles.projectAdministrator.description',
        'Projects, their members and their guarded settings. A project administrator can still add a second account or another project administrator to a project: the access.admin_membership_granted signal detects this, it does not prevent it.',
      ),
  },
  security_auditor: {
    label: () => t('pages.admin.users.roles.securityAuditor.label', 'Security auditor'),
    description: () =>
      t(
        'pages.admin.users.roles.securityAuditor.description',
        'Reads signals, the audit trail and the access history, and attests break-glass reviews.',
      ),
  },
  emergency_access: {
    label: () => t('pages.admin.users.roles.emergencyAccess.label', 'Emergency access'),
    description: () =>
      t('pages.admin.users.roles.emergencyAccess.description', 'A person who may ask for break-glass.'),
  },
  editor: {
    label: () => t('pages.admin.users.roles.editor.label', 'Editor'),
    description: () =>
      t('pages.admin.users.roles.editor.description', 'A legacy role. It keeps the permissions it held at the upgrade.'),
  },
  viewer: {
    label: () => t('pages.admin.users.roles.viewer.label', 'Viewer'),
    description: () =>
      t('pages.admin.users.roles.viewer.description', 'A legacy role. It keeps the permissions it held at the upgrade.'),
  },
};

/**
 * The label of a role. `role` is what the server said about it; without it
 * (the CSV export, a role the matrix did not list) a name with built-in copy
 * is labelled by it.
 */
export function adminRoleLabel(name: string, role?: AdministrationRole): string {
  if (role !== undefined && !role.builtIn) return name;
  return BUILT_IN_ROLE_COPY[name]?.label() ?? role?.displayName ?? name;
}

/** The one-line description of a built-in role; none for a custom one. */
export function adminRoleDescription(role: AdministrationRole): string | undefined {
  return role.builtIn ? BUILT_IN_ROLE_COPY[role.name]?.description() : undefined;
}
