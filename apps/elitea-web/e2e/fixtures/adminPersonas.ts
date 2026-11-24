/**
 * The built-in role personas (ADR-0059 R3.13a): one end-to-end user per
 * built-in administration role, each holding exactly that role.
 *
 * `scripts/e2e-stack.sh seed` creates them (the oidc-mock users and the SQL
 * block "built-in role personas") and fails when a role is missing;
 * `e2e/auth.setup.ts` signs each in once and saves its storage state at
 * `STORAGE_STATE.personas[role]` (`playwright.config.ts`). A persona journey
 * selects one with `test.use({ storageState: STORAGE_STATE.personas.llm_manager })`.
 *
 * The existing admin persona (`STORAGE_STATE.admin`, `e2e-admin`) is NOT one of
 * these: it holds `admin` plus the custom role `e2e_admin_extras`, and the
 * admin journeys keep using it. `scripts/e2e-journey-shape.test.mjs` ("ADR-0059
 * R3.13a") holds this list, the seed and the setup to one another.
 *
 * No imports: `playwright.config.ts` reads this module.
 */

/** A built-in administration role with a declared set. */
export type BuiltInAdminRole =
  | 'super_admin'
  | 'admin'
  | 'llm_manager'
  | 'project_administrator'
  | 'security_auditor'
  | 'emergency_access';

export interface AdminPersona {
  readonly role: BuiltInAdminRole;
  readonly email: string;
  readonly name: string;
}

/** In console order (internal/adminaccess/builtin_roles.go). */
export const ADMIN_PERSONAS: readonly AdminPersona[] = [
  { role: 'super_admin', email: 'e2e-global-admin@autotest.local', name: 'E2E Global Admin' },
  { role: 'admin', email: 'e2e-platform-admin@autotest.local', name: 'E2E Platform Admin' },
  { role: 'llm_manager', email: 'e2e-llm-manager@autotest.local', name: 'E2E LLM Manager' },
  {
    role: 'project_administrator',
    email: 'e2e-project-administrator@autotest.local',
    name: 'E2E Project Administrator',
  },
  { role: 'security_auditor', email: 'e2e-auditor@autotest.local', name: 'E2E Auditor' },
  { role: 'emergency_access', email: 'e2e-emergency@autotest.local', name: 'E2E Emergency' },
];

/** The storage-state file name of a persona, inside the state directory. */
export function personaStateFile(role: BuiltInAdminRole): string {
  return `persona-${role.replaceAll('_', '-')}.json`;
}
