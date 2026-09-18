/**
 * "The signed-in user's personal project id" — the baseline's
 * `useSelector(state => state.user).personal_project_id`.
 *
 * A credential picker needs it for one thing only: to tell a PERSONAL saved
 * credential from a project one. The baseline reads the two apart the same way
 * (`isConfigurationPersonal` in `apps/elitea-ui/src/[fsd]/features/credentials/
 * ui/credentials-select/CredentialsSelect.jsx`), and the toolkit settings store
 * the answer back as the `private` half of `{elitea_title, private}`.
 *
 * Read through the SAME router-context seam as `./useSelectedProjectId.ts`
 * (`RouterContext.auth.getUser()`, unit R1/R2). `pages/` may not import
 * `src/app/` (`no-upward-from-pages`), so the session store is not reachable
 * from here directly.
 */
import { useRouteContext } from '@tanstack/react-router';

interface PersonalProjectIdContext {
  readonly auth?: {
    readonly getUser?: () => { readonly personal_project_id?: string } | undefined;
  };
}

function isPersonalProjectIdContext(value: unknown): value is PersonalProjectIdContext {
  return typeof value === 'object' && value !== null;
}

/** Pure extraction, unit-tested directly (no router needed) — the hook below is a one-line wrapper over this. */
export function selectPersonalProjectId(context: unknown): string | undefined {
  if (!isPersonalProjectIdContext(context)) return undefined;
  return context.auth?.getUser?.()?.personal_project_id;
}

export function usePersonalProjectId(): string | undefined {
  const context: unknown = useRouteContext({ strict: false });
  return selectPersonalProjectId(context);
}

/**
 * "The selected project is not my personal one" — the same derivation
 * `routes/-lib/useCredentialFormContext.ts` already uses for its own
 * `isTeamProject`, reused here (#952/ELITEA-1092,1094,1099) so
 * `EditToolkit.tsx` can thread it to `ToolkitForm` without adding its own
 * inline boolean expression (§3.5 complexity budget). Deliberately `false`
 * while either id is unknown: this only unlocks a warning modal, never a
 * restriction. Not exported — `useIsTeamProject` below is the only caller,
 * and `knip` flags an unused export otherwise.
 */
function isTeamProject(projectId: string | undefined, personalProjectId: string | undefined): boolean {
  return projectId !== undefined && projectId !== '' && personalProjectId !== undefined && projectId !== personalProjectId;
}

export function useIsTeamProject(projectId: string | undefined): boolean {
  const personalProjectId = usePersonalProjectId();
  return isTeamProject(projectId, personalProjectId);
}

/**
 * #902/ELITEA-0726: the same question answered in THREE values — `undefined`
 * while either id is still unknown. `useIsTeamProject` collapses that unknown
 * to `false` on purpose (it only unlocks a warning modal, never a
 * restriction); a user-visible label must not, or it claims a scope nobody
 * established. Pure half below so it is unit-testable without a router.
 */
export function selectIsTeamProject(projectId: string | undefined, personalProjectId: string | undefined): boolean | undefined {
  if (projectId === undefined || projectId === '' || personalProjectId === undefined) return undefined;
  return projectId !== personalProjectId;
}

export function useProjectScopeIsTeam(projectId: string | undefined): boolean | undefined {
  return selectIsTeamProject(projectId, usePersonalProjectId());
}
