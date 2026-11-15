import { useGetApplication, useGetApplicationVersionDetail } from '@/shared/api/generated/applications/applications';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type {
  ApplicationDetail,
  ApplicationVersionDetail,
  ApplicationVersionSummary,
} from '@/shared/api/generated/model';

/** Stable empty array — `detail?.versions ?? []` would otherwise create a fresh array reference every render (`react-hooks/exhaustive-deps`: a `useMemo` depending on it would never actually memoize). */
const EMPTY_VERSIONS: readonly ApplicationVersionSummary[] = [];

export interface EditApplicationData {
  readonly detail: ApplicationDetail | undefined;
  readonly versions: readonly ApplicationVersionSummary[];
  readonly activeVersion: ApplicationVersionDetail | undefined;
  readonly isFetching: boolean;
  readonly isError: boolean;
  /**
   * `true` when the application-DETAIL fetch itself (not the optional
   * explicit-version fetch) failed with a 404 or 400 — old app:
   * `isNotFoundError` (`common/utils.jsx:143`,
   * `err?.status === 404 || err?.status === 400`), read by
   * `EditApplication.jsx:63` as `shouldShowNotFoundPage = (isError &&
   * isNotFoundError(error)) || isVersionNotFound`. This app's transport
   * throws `EliteaApiError` (`shared/api/generated/mutator.ts`) instead of
   * resolving with an RTK-Query-shaped `{status}` object — same status-code
   * check, adapted to the real thrown shape (`error.failure.kind ===
   * 'http'`), same "duck-typed against EliteaApiError" convention
   * `useSharepointCheckConnection.hooks.ts`'s `isEliteaApiErrorLike`/
   * `pages/settings/Secrets.tsx`'s `isForbiddenError` already establish.
   */
  readonly isDetailNotFound: boolean;
}

/** Split out purely to keep `useEditApplicationData`'s own branch count under the oxlint complexity budget — see `EditApplicationData.isDetailNotFound`'s own doc comment for the full citation trail. */
function isNotFoundApiError(error: unknown): boolean {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return false;
  return error.failure.status === 404 || error.failure.status === 400;
}

/** Split out purely to keep `useEditApplicationData`'s own branch count under the oxlint complexity budget. */
function needsExplicitVersionFetch(
  requestedVersionId: string | undefined,
  currentVersionId: string | undefined,
): boolean {
  return requestedVersionId !== undefined && requestedVersionId !== currentVersionId;
}

/** Split out purely to keep `useEditApplicationData`'s own branch count under the oxlint complexity budget. */
function isVersionQueryEnabled(
  needsExplicit: boolean,
  projectId: string | undefined,
  applicationId: number | undefined,
  explicitVersionId: number,
): boolean {
  if (!needsExplicit) return false;
  if (projectId === undefined || applicationId === undefined) return false;
  return !Number.isNaN(explicitVersionId);
}

/** Split out purely to keep `useEditApplicationData`'s own branch count under the oxlint complexity budget — `isFetching`/`isError` are both this same "primary query's flag, OR the secondary query's flag when it's actually in play" shape. */
function combineQueryFlag(primary: boolean, needsExplicit: boolean, secondary: boolean): boolean {
  if (primary) return true;
  return needsExplicit && secondary;
}

/** Split out purely to keep `useEditApplicationData`'s own branch count under the oxlint complexity budget. */
function isDetailQueryEnabled(projectId: string | undefined, applicationId: number | undefined): boolean {
  return projectId !== undefined && applicationId !== undefined;
}

/** Split out purely to keep `useEditApplicationData`'s own branch count under the oxlint complexity budget. */
function resolveExplicitVersionId(needsExplicit: boolean, requestedVersionId: string | undefined): number {
  if (!needsExplicit || requestedVersionId === undefined) return 0;
  return Number(requestedVersionId);
}

/** Split out purely to keep `useEditApplicationData`'s own branch count under the oxlint complexity budget. */
function pickActiveVersion(
  needsExplicit: boolean,
  explicitVersion: ApplicationVersionDetail | undefined,
  defaultVersion: ApplicationVersionDetail | undefined,
): ApplicationVersionDetail | undefined {
  return needsExplicit ? explicitVersion : defaultVersion;
}

/**
 * Split out of `EditApplication.tsx` purely to keep that page's own
 * cyclomatic complexity under the oxlint budget (12) and its line count
 * under the §3.5 400-line file budget — this hook alone combines the
 * two-query (application detail + optional explicit-version) fetch/enable/
 * loading/error branching the baseline flattens into one component. The
 * four helper functions above exist for the same reason (this hook's OWN
 * branch count was still over budget with them inlined).
 */
export function useEditApplicationData(
  projectId: string | undefined,
  applicationId: number | undefined,
  requestedVersionId: string | undefined,
): EditApplicationData {
  const detailQuery = useGetApplication(projectId ?? '', applicationId ?? 0, {
    query: { enabled: isDetailQueryEnabled(projectId, applicationId) },
  });
  // `.data.data`'s declared type includes the error-envelope variant — never
  // actually reachable here since `eliteaFetch` throws instead of resolving
  // with it (mutator.ts's §3.6 unwrap contract).
  const detail = detailQuery.data?.data as ApplicationDetail | undefined;
  const defaultVersion = detail?.version_details;

  const needsExplicit = needsExplicitVersionFetch(requestedVersionId, defaultVersion?.id);
  const explicitVersionId = resolveExplicitVersionId(needsExplicit, requestedVersionId);
  const versionQuery = useGetApplicationVersionDetail(projectId ?? '', applicationId ?? 0, explicitVersionId, {
    query: { enabled: isVersionQueryEnabled(needsExplicit, projectId, applicationId, explicitVersionId) },
  });
  const explicitVersion = versionQuery.data?.data as ApplicationVersionDetail | undefined;

  return {
    detail,
    versions: detail?.versions ?? EMPTY_VERSIONS,
    activeVersion: pickActiveVersion(needsExplicit, explicitVersion, defaultVersion),
    isFetching: combineQueryFlag(detailQuery.isFetching, needsExplicit, versionQuery.isFetching),
    isError: combineQueryFlag(detailQuery.isError, needsExplicit, versionQuery.isError),
    isDetailNotFound: isNotFoundApiError(detailQuery.error),
  };
}
