/**
 * Re-reads the two lists the shell is built from, once, when first-login
 * provisioning turns out to have finished AFTER they were asked for.
 *
 * ## The race
 *
 * On a fresh install the browser asks for everything at mount. The project
 * list (`GET /projects/project/default/{publicProjectId}`) and the permission
 * list (`GET /auth/permissions/prompt_lib/{projectId}`) are answered against a
 * user who has no personal project yet, so both come back empty.
 * `GET /social/author` is the one request that PROVISIONS, and it names the
 * project only once that work is done — later than the two lists, and, when a
 * second author request is in flight beside it, later still (see
 * `usePersonalProjectId`).
 *
 * Nothing then re-asked. `QUERY_DEFAULT_OPTIONS.staleTime` is 30s and both
 * queries had SUCCEEDED, so the empty answers stood: the project switcher read
 * "Project: No projects" — `ProjectSwitcher` names the selected project by
 * finding it in the list, and the list had nothing to find — and
 * `visibleNavSections` dropped every permission-gated row, so Chats, Toolkits,
 * MCPs, Credentials and Artifacts all disappeared. Only a manual page reload
 * fixed it, which is the shape of a cache that was right about what it was
 * told and was never told again.
 *
 * ## The gate
 *
 * The retry fires only on the evidence of that race: the author query has named
 * a personal project, the project list has settled, and the project is not in
 * it. On an ordinary login the list already carries it and this hook does
 * nothing at all, so a normal page load pays no extra round trip.
 *
 * THE LIST ARM DOES NOT WAIT FOR A SELECTION. It used to, and that made it unreachable
 * on the very race it was written for. `AppShell`'s auto-select effect refuses
 * to select anything while the author query names no project, and on a first
 * login that query is answered `personal_project_id: ""` — the provisioning
 * request that wins the boot race is `app/session-store.ts`'s, and the loser
 * has no attempt of its own to wait for. So nothing was selected, this hook's
 * `selectedProjectId !== personalProjectId` gate could never come true, and
 * the two hooks each waited for the other. Measured that way on the standalone
 * stack: neither the list nor the permission list was ever asked a second
 * time, and no `el.project.*` key was ever written.
 *
 * `usePersonalProjectId` now keeps asking until the server names the project,
 * so the two effects run in the order the shell needs: the list is re-read on
 * the evidence that it is stale, and the auto-select effect selects out of the
 * answer.
 *
 * Each arm fires at most ONCE per `personal_project_id`, held by a ref. A
 * deployment whose project listing genuinely never carries the personal project
 * therefore costs one extra pair of requests per session, not a loop.
 *
 * Both lists are re-read because one cause produced both empty answers.
 * Re-reading only the projects query would settle the switcher's label and
 * leave the nav still truncated.
 *
 * ## Why `resetQueries` and not `invalidateQueries`
 *
 * The permission request can still be IN FLIGHT at this moment: it is issued
 * the instant a project is selected, which may be the same commit this effect
 * runs in. `invalidateQueries` cannot replace an in-flight FIRST fetch.
 * `Query.fetch` honours `cancelRefetch` only when the query already holds data
 * (`query-core/build/modern/query.js`: `if (this.state.data !== undefined &&
 * fetchOptions?.cancelRefetch) this.cancel(...)`, `else if (this.#retryer)
 * return this.#retryer.promise`) — so on a pending query it silently returns
 * the very promise carrying the too-early answer, and the invalidation resolves
 * having changed nothing. Measured exactly that way: the projects query, which
 * had settled, refetched; the permission query, which had not, did not.
 *
 * `resetQueries` calls `Query.reset()` first, which destroys the retryer and
 * puts the query back at `fetchStatus: 'idle'` before refetching, so a pending
 * first fetch is replaced instead of awaited. The data it discards is the empty
 * answer this hook exists to discard.
 */
import { useCallback, useEffect, useRef } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import type { Project } from '@/entities/project';
import { getListProjectsQueryKey } from '@/shared/api/generated/applications/applications';

/**
 * `getPermissionListQueryKey` builds `['/auth/permissions/prompt_lib/{id}']`,
 * one key per project. Which project the shell ends up reading permissions for
 * is not this hook's to decide — a selection persisted by an earlier session is
 * answered for too — so the whole family is matched by prefix rather than by
 * building one key here.
 */
const PERMISSION_KEY_PREFIX = '/auth/permissions/prompt_lib/';

export interface SettleShellAfterProvisioningArgs {
  /** `personal_project_id` from `GET /social/author`, once it resolves. */
  readonly personalProjectId: string | undefined;
  /** `vite_public_project_id` — the project the listing is scoped by. */
  readonly publicProjectId: string;
  readonly projects: readonly Project[];
  /** True while the project list is in flight. An unsettled list is evidence of nothing. */
  readonly projectsLoading: boolean;
  /**
   * The project the shell has actually selected.
   *
   * It is not a gate on the FIRST arm — waiting for it there is what made this
   * hook unreachable — but it is what the SECOND arm waits for. The permission
   * query is `usePermissionSet(project?.id)`, so it does not exist until a
   * project is selected, and a reset issued before that has nothing to reset.
   */
  readonly selectedProjectId: string | undefined;
}

export function useSettleShellAfterProvisioning({
  personalProjectId,
  publicProjectId,
  projects,
  projectsLoading,
  selectedProjectId,
}: SettleShellAfterProvisioningArgs): void {
  const queryClient = useQueryClient();
  const settledFor = useRef<string | undefined>(undefined);
  const permissionsSettledFor = useRef<string | undefined>(undefined);

  const resetPermissionFamily = useCallback(() => {
    void queryClient.resetQueries({
      predicate: (query) => {
        const [key] = query.queryKey;
        return typeof key === 'string' && key.startsWith(PERMISSION_KEY_PREFIX);
      },
    });
  }, [queryClient]);

  // Arm one — the project list. It answered before the project existed.
  useEffect(() => {
    if (personalProjectId === undefined || personalProjectId === '') return;
    if (projectsLoading) return;
    if (settledFor.current === personalProjectId) return;
    if (projects.some((candidate) => String(candidate.id) === personalProjectId)) return;
    settledFor.current = personalProjectId;

    const numericPublicProjectId = Number(publicProjectId);
    if (Number.isFinite(numericPublicProjectId)) {
      void queryClient.resetQueries({ queryKey: getListProjectsQueryKey(numericPublicProjectId) });
    }
    resetPermissionFamily();
  }, [personalProjectId, publicProjectId, projects, projectsLoading, queryClient, resetPermissionFamily]);

  // Arm two — the permission list, once the project is SELECTED.
  //
  // One cause produced both empty answers, and re-reading only the projects
  // query settles the switcher's label while the nav stays truncated. But the
  // permission query is born from the selection, so at the moment arm one runs
  // there may be nothing to reset yet — and the fetch that follows the
  // selection can still be the one that raced.
  //
  // Gated on arm one having fired for this same project, so it costs nothing
  // on an ordinary login: no race, no reset, no extra request.
  useEffect(() => {
    if (personalProjectId === undefined || personalProjectId === '') return;
    if (settledFor.current !== personalProjectId) return;
    if (selectedProjectId !== personalProjectId) return;
    if (permissionsSettledFor.current === personalProjectId) return;
    permissionsSettledFor.current = personalProjectId;
    resetPermissionFamily();
  }, [personalProjectId, selectedProjectId, resetPermissionFamily]);
}
