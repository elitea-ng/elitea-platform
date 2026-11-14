import { useMemo } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';

/**
 * Local, `features/pipelines`-owned duplicate of `features/agents/lib/
 * useHasPermission.ts` (itself the equivalent of the baseline's
 * `useCheckPermission().checkPermission`,
 * `apps/elitea-ui/src/hooks/useCheckPermission.js`), used by
 * `PipelineEditor.tsx` to gate on `PERMISSIONS.applications.update` the
 * same way the baseline's `PipelineEditor.jsx:143-146` does.
 *
 * Duplicated, not imported: `no-sideways-features` forbids `features/
 * pipelines` reaching into `features/agents` even though the two files
 * would be byte-for-byte identical — see `agentEditorViewState`'s
 * `PUBLIC_PROJECT_ID`/`useSelectedProjectId.ts` in this same slice for the
 * established precedent of duplicating a small cross-domain primitive
 * rather than reaching sideways for it.
 */
export function useHasPermission(projectId: string | undefined, permission: string): boolean {
  return usePermissionState(projectId, permission).allowed;
}

/** `settled` is false until the permission list answered (or failed), so a caller can tell "not yet known" from "no". */
export function usePermissionState(projectId: string | undefined, permission: string): { readonly allowed: boolean; readonly settled: boolean } {
  const query = usePermissionList(projectId ?? '', { query: { enabled: projectId !== undefined } });

  const permissions = useMemo(() => {
    // `query.data.data`'s declared type includes the error-envelope variant —
    // never actually reachable here, since `eliteaFetch` throws instead of
    // resolving with it (mutator.ts's §3.6 unwrap contract; same cast
    // convention `features/agents/lib/useHasPermission.ts` already established).
    const list = query.data?.data as Permission[] | undefined;
    if (!list) return new Set<string>();
    return new Set(list.filter((entry) => entry.enabled).map((entry) => entry.name));
  }, [query.data]);

  return { allowed: permissions.has(permission), settled: projectId !== undefined && !query.isPending };
}
