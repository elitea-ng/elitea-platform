/**
 * "Is the SELECTED project the caller's own personal project?" — the gate the
 * reference writes as `isPrivateProject = projectId == user.personal_project_id`
 * (`apps/elitea-ui/src/[fsd]/pages/settings/index.jsx:162`), corrected for the
 * one way this backend's `personal_project_id` differs from pylon's.
 *
 * THE ID COMPARISON ALONE IS NOT ENOUGH HERE. `GET /social/author` resolves
 * `personal_project_id` down three branches (`resolvePersonalProjectID`,
 * services/elitea-main/internal/api/v2/social/handler.go). Branches 1 and 2
 * name a real `project_user_<uid>` row, exactly as pylon's
 * `projects_get_personal_project_id` did. Branch 3 — "the lowest-id project the
 * user actually holds a role in" — does not: it exists so an account whose
 * personal project has not been provisioned yet still gets a usable scope
 * instead of a 403, and it hands back an ORDINARY TEAM PROJECT.
 *
 * Provisioning only runs when the resolver answers "" (GetAuthor's
 * `if resp.PersonalProjectID == "" && h.ensurePersonalProject(...)`), so an
 * account that reaches branch 3 keeps reaching it: for every member of a single
 * shared project, `personal_project_id` IS that shared project, for good.
 *
 * Read as an id comparison, that made the Settings drawer drop the Users tab
 * and redirect `/settings/users` back to General for those accounts — a project
 * with a membership to manage, presented as a one-person project.
 * `widgets/sidebar/lib/projectOptions.ts` had already hit the same edge from
 * the other side (it renamed such a project to "Private") and fixed it the same
 * way: require BOTH the addressed id AND the reserved storage name.
 */
import { useMemo } from 'react';

import { useListProjects } from '@/shared/api/generated/applications/applications';
import type { ProjectWithGroups } from '@/shared/api/generated/model';
import { isPersonalProjectName } from '@/entities/project';
import { getConfig } from '@/shared/config';

/**
 * `true` only when the selected project is the addressed personal project AND
 * its stored name is the reserved `project_user_<uid>`.
 *
 * The project list is the same query `widgets/app-shell` already runs with the
 * same key, so this costs no extra request. While it is unresolved — loading,
 * disabled because the deployment publishes no usable public project id, or
 * failed — the answer is `false`. That is the safe direction for every caller
 * so far: it shows a tab that a personal project would hide, rather than
 * hiding (and redirecting away from) a tab a shared project needs.
 */
export function useIsPersonalProject(
  selectedProjectId: string | undefined,
  personalProjectId: string | undefined,
): boolean {
  const configResult = getConfig();
  const publicProjectId = configResult.status === 'ok' ? configResult.config.vite_public_project_id : '';
  const numericPublicProjectId = Number(publicProjectId);
  const query = useListProjects(numericPublicProjectId, undefined, {
    query: { enabled: Number.isFinite(numericPublicProjectId) && publicProjectId !== '' },
  });

  return useMemo(() => {
    if (personalProjectId === undefined || personalProjectId === '') return false;
    if (selectedProjectId === undefined || selectedProjectId === '') return false;
    if (String(selectedProjectId) !== String(personalProjectId)) return false;
    // `query.data.data`'s declared type includes the 401 envelope variant,
    // which `eliteaFetch` throws on rather than resolving with — the same
    // narrowing `widgets/sidebar/api/useProjectOptions.ts` documents.
    const list = query.data?.data as ProjectWithGroups[] | undefined;
    const selected = list?.find((project) => String(project.id) === String(selectedProjectId));
    if (selected === undefined) return false;
    return isPersonalProjectName(selected.name);
  }, [query.data, selectedProjectId, personalProjectId]);
}
