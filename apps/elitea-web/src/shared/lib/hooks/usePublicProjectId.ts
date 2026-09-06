/**
 * The id of the platform's PUBLIC project, as the server resolves it.
 *
 * ## Why this exists
 *
 * One project was configured in three places that could not check each other:
 * `ELITEA_AI_PROJECT_ID` in elitea-main, the identically named variable in the
 * LLM gateway, and `VITE_PUBLIC_PROJECT_ID` written into `config.js` by this
 * image's `docker-entrypoint.sh`. Nothing compared the SPA's copy with the
 * server's, and one page did not even read the SPA's copy: `pages/settings/
 * ServicePrompts.tsx` compared the selected project against the literal `'1'`
 * and gated its queries on the result, so on a deployment whose public project
 * is not id 1 the Service Prompts cards rendered empty — no request made, no
 * error shown, nothing in the console.
 *
 * elitea-main now publishes the resolved id on `platform_settings`, the
 * endpoint the app shell already polls, so the answer costs no extra request.
 * That value is authoritative here.
 *
 * ## The build-time copy is the FALLBACK, not the source
 *
 * `vite_public_project_id` stays, for two reasons that are not nostalgia:
 *
 *  - It is a REQUIRED runtime-config key (`shared/config`'s schema), so the app
 *    already refuses to start without it; removing it would be a separate
 *    change to the container contract with its own operator migration.
 *  - `platform_settings` is a query. It is in flight on the first render and it
 *    can fail. A hook that answered `''` in those two states would tell every
 *    caller "this is not the public project", which is the silent-wrong answer
 *    this whole change exists to remove.
 *
 * So: prefer the server, fall back to the image, and never invent `'1'`.
 *
 * ## A string, though the server sends a number
 *
 * Every comparison in this app is against a project id the selected-project
 * store holds as a string, and `entities/project`'s `isPublicProject` selector
 * takes either. Converting once, here, keeps one representation in the callers.
 */
import { useGetPlatformSettings } from '@/shared/api/generated/admin/admin';
import { getConfig } from '@/shared/config';

/**
 * `platform_settings` declares `additionalProperties: true`, so this key is a
 * real, always-present response field that the generated `PlatformSettings`
 * type does not name — the same route `usePlatformAnnouncements` and
 * `useIsAnalyticsVisible` take. Reading it through a narrow local shape keeps
 * the untyped access in one validating function.
 */
interface RawPublicProject {
  readonly public_project_id?: unknown;
}

/** Accepts the number the server sends, and a numeric string, and nothing else. */
function normalisePublicProjectId(raw: unknown): string {
  if (typeof raw === 'number') {
    return Number.isInteger(raw) && raw > 0 ? String(raw) : '';
  }
  if (typeof raw === 'string' && /^[1-9][0-9]*$/.test(raw)) return raw;
  return '';
}

/** The build-time copy in `config.js`, or `''` when the config never resolved. */
function configuredPublicProjectId(): string {
  const config = getConfig();
  return config.status === 'ok' ? config.config.vite_public_project_id : '';
}

/**
 * NOT exported. The only thing any caller has needed so far is the comparison
 * below, and an export with no importer is what the dead-code gate is for.
 * Export it the first time a caller needs the id itself.
 */
function usePublicProjectId(): string {
  const query = useGetPlatformSettings();
  // `.data.data` — the enveloped shape every generated read resolves to;
  // `eliteaFetch` throws rather than resolving with the error variant (§3.6).
  const raw = (query.data?.data ?? undefined) as RawPublicProject | undefined;
  return normalisePublicProjectId(raw?.public_project_id) || configuredPublicProjectId();
}

/**
 * Whether `projectId` is the public project, by the answer above.
 *
 * Returns false for an empty `projectId` and for an unresolvable public id,
 * rather than letting `'' === ''` report the two unknowns as a match.
 */
export function useIsPublicProject(projectId: string | null | undefined): boolean {
  const publicProjectId = usePublicProjectId();
  if (!projectId || !publicProjectId) return false;
  return String(projectId) === publicProjectId;
}
