import type { Project } from './types';

/**
 * Recurring inline predicate in the old app
 * (`projectId == PUBLIC_PROJECT_ID`), e.g. apps/elitea-ui/src/GA.js:36,
 * apps/elitea-ui/src/[fsd]/app/routes/SkillsGuard.jsx:13. The reserved
 * public/marketplace project id is NOT a fixed constant — it is
 * `VITE_PUBLIC_PROJECT_ID`, a per-deployment runtime-config value
 * (apps/elitea-ui/src/common/constants.js:14,61: `PUBLIC_PROJECT_ID =
 * +VITE_PUBLIC_PROJECT_ID`) — so it is a required parameter here rather than
 * an invented in-package constant; `shared/config` (unit F3) is the source
 * of the real value at the call site. The old app compares with `==` (the
 * id may arrive as a string from a route param); this selector normalises
 * both sides to string for the same effect without a loose-equality lint
 * violation.
 */
export function isPublicProject(projectId: number | string, publicProjectId: number | string): boolean {
  return String(projectId) === String(publicProjectId);
}

/**
 * `suspended` is the only suspension signal the server sends. The spec also
 * declared a `status` enum, but internal/api/v2/projects/handler.go never
 * emits it, so the old `project.status === 'suspended'` arm compared
 * `undefined` and could never be true.
 */
export function isSuspendedProject(project: Project): boolean {
  return project.suspended;
}

/** Alphabetical name sort, case-insensitive. */
export function sortProjectsByName(projects: readonly Project[]): Project[] {
  return [...projects].sort((a, b) => a.name.toLowerCase().localeCompare(b.name.toLowerCase()));
}

/**
 * pylon's reserved storage name for a personal project
 * (`PROJECT_PERSONAL_NAME_TEMPLATE`, `project_user_<uid>`) — the same rule the
 * admin projects listing uses to answer `is_personal`, and the same one
 * `personalproject.Name` writes on this backend.
 */
const PERSONAL_STORAGE_NAME = /^project_user_\d+$/;

/**
 * `true` when a project's STORED name marks it as somebody's personal project.
 *
 * WHY THE NAME AND NOT THE ID. `GET /social/author`'s `personal_project_id` is
 * the id the old app compares against, and behind pylon that field only ever
 * named a `project_user_<uid>` row. This backend used to resolve the same field
 * down a third branch pylon does not have — "the lowest-id project the user
 * actually holds a role in" (`resolvePersonalProjectID`, services/elitea-main/
 * internal/api/v2/social/handler.go) — so an account with no personal project
 * yet was handed an ORDINARY TEAM PROJECT, and every "is this my private
 * project?" test written as an id comparison alone answered yes for a shared
 * one. That branch is gone (issue 843): the field is now either a real
 * `project_user_<uid>` id or empty. The NAME rule stays the rule here, because
 * it is the one that does not depend on which endpoint answered.
 *
 * `widgets/sidebar/lib/projectOptions.ts` already had to learn this (it was
 * renaming a shared project to "Private"). The Settings drawer learned it the
 * same way: it hid the Users tab — and redirected away from `/settings/users` —
 * for every member of a single shared project.
 */
export function isPersonalProjectName(name: string): boolean {
  return PERSONAL_STORAGE_NAME.test(name);
}
