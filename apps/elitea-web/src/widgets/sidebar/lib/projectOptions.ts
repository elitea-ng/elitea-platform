/**
 * Project-switcher ordering (old app: `components/ProjectSelect.jsx`'s
 * `projectOptions` useMemo — the public project pinned first, then the rest
 * alphabetically). Reduced scope, documented:
 *
 *  - The caller's private/personal project is pinned second. Its identity
 *    comes from `GET /social/author`'s `personal_project_id`, the same source
 *    the old app used. Project-list rows do not carry this marker themselves.
 *  - The reserved public and personal storage names (`promptlib_public` and
 *    `project_user_<id>`) are presentation details. The old app renders them
 *    as `Public` and `Private`; the new UI must not expose them to users.
 *
 *    THE SUBSTITUTION IS GATED ON THE STORED NAME, not on the id alone. The
 *    old app rewrites by id (`getProjectName`: `if (item.id == privateProjectId)
 *    return PRIVATE_PROJECT_NAME`) because behind pylon `personal_project_id`
 *    only ever names a `project_user_<uid>` row — `projects_get_personal_
 *    project_id` (legacy/plugins/projects/rpc/poc.py) answers that row or
 *    NOTHING. This backend resolves the same field down a third branch pylon
 *    does not have: "the lowest-id project the user actually holds a role in"
 *    (`resolvePersonalProjectID`, services/elitea-main/internal/api/v2/social/
 *    handler.go), which it needs so an account whose personal project has not
 *    been provisioned yet still gets a usable scope instead of a 403. That
 *    branch hands back an ORDINARY TEAM PROJECT, and rewriting by id alone
 *    then replaced its real name with "Private" — the member of one shared
 *    project saw their project's name destroyed, and the E2E journeys that
 *    read "Project: Default Project" out of the switcher, the analytics header
 *    and the share-link reload (J2/J6/J7/J24) all read "Private" instead.
 *
 *    So a row is renamed only when it is BOTH the addressed project AND still
 *    carries the reserved storage name that has no business on screen. A real
 *    name is never overwritten; such a project is still PINNED, which is the
 *    half of the old behaviour that is about ordering rather than identity.
 *  - `filterIds`/`hasPublicProjectAccess` (old app: gates whether the
 *    public project even appears) are dropped — `hasPublicProjectAccess`
 *    reads a permission this port has no evidence for
 *    (`usePublicProjectAccessCheck`, `[fsd]/features/project/lib/hooks`, not
 *    ported). The public project is always included when the API returns
 *    it.
 */
import type { Project } from '@/entities/project';
import { isPersonalProjectName, isPublicProject, sortProjectsByName } from '@/entities/project';

/** pylon's stored name for the shared public project (`PROJECT_PUBLIC_NAME`). */
const PUBLIC_STORAGE_NAME = 'promptlib_public';

/** Swaps a reserved storage name for its user-facing label, leaving any real name alone. */
function withDisplayName(project: Project, reserved: boolean, label: string): Project {
  return reserved ? { ...project, name: label } : project;
}

export function orderedProjectOptions(
  projects: readonly Project[],
  publicProjectId: string,
  personalProjectId?: string,
): readonly Project[] {
  const namedProjects = projects.map((project) => {
    if (isPublicProject(project.id, publicProjectId)) {
      return withDisplayName(project, project.name === PUBLIC_STORAGE_NAME, 'Public');
    }
    if (personalProjectId !== undefined && String(project.id) === personalProjectId) {
      return withDisplayName(project, isPersonalProjectName(project.name), 'Private');
    }
    return project;
  });
  const publicProject = namedProjects.find((project) => isPublicProject(project.id, publicProjectId));
  const privateProject = namedProjects.find(
    (project) =>
      personalProjectId !== undefined &&
      String(project.id) === personalProjectId &&
      project !== publicProject,
  );
  const rest = sortProjectsByName(
    namedProjects.filter((project) => project !== publicProject && project !== privateProject),
  );
  return [publicProject, privateProject, ...rest].filter((project): project is Project => project !== undefined);
}
