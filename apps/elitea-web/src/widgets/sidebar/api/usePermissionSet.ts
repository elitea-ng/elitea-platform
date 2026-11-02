/**
 * Thin wrapper over the generated `usePermissionList` hook (S4, `shared/api/
 * generated/auth/auth.ts`; endpoint `GET /auth/permissions/prompt_lib/
 * {projectId}`), reduced to the `Set<string>` shape every permission check
 * in this widget actually wants — mirrors old
 * app's `new Set(permissions)` (`SidebarBody.jsx:80`).
 *
 * `enabled: projectId !== undefined` (react-query's own opt-out, not a
 * hand-rolled skip flag) — matches the generated hook's `projectId: string`
 * (non-optional) parameter; this widget only knows the selected project
 * once `widgets/app-shell`'s store has one.
 *
 * `query.data` is the enveloped `{data, status, headers}` shape declared by
 * `permissionListResponse` — `eliteaFetch` (`shared/api/generated/
 * mutator.ts`) was fixed at the source (2026-07-27) to actually build that
 * envelope rather than resolving with the bare body; this reads through
 * `.data`. Failures surface through `query.isError`/`query.error` (a thrown
 * `EliteaApiError`), never a `{status: 401}` value.
 */
import { useMemo } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';

export function usePermissionSet(projectId: string | undefined): ReadonlySet<string> {
  const query = usePermissionList(projectId ?? '', { query: { enabled: projectId !== undefined } });

  return useMemo(() => {
    // `query.data.data`'s declared type includes the error-envelope variant
    // — never actually reachable here, since `eliteaFetch` throws instead
    // of resolving with it (mutator.ts's §3.6 unwrap contract).
    const permissions = query.data?.data as Permission[] | undefined;
    if (!permissions) return new Set<string>();
    return new Set(permissions.filter((permission) => permission.enabled).map((permission) => permission.name));
  }, [query.data]);
}
