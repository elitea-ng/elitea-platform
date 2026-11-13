import type { VersionSummary } from './types';

/**
 * apps/elitea-ui/src/[fsd]/entities/version/lib/constants/version.constants.js:1
 * — the well-known "unnamed default" version name, mirroring the backend's
 * `get_default_version()` fallback.
 */
export const LATEST_VERSION_NAME = 'base';

/**
 * Resolves the "current/default" version out of a version list, matching the
 * three duplicated call sites in the old app (apps/elitea-ui/src/[fsd]/
 * entities/application-tab-bar/ui/ApplicationVersionSelect.jsx:56-61;
 * entities/skill-tab-bar/ui/SkillTabBar.jsx:18-24; hooks/application/
 * useDeleteVersion.js:21-23): prefer the version whose id matches
 * `defaultVersionId` (from the owning entity's `meta.default_version_id`);
 * otherwise fall back to the version named `LATEST_VERSION_NAME` ("base");
 * otherwise `undefined` (no default resolvable).
 */
export function selectDefaultVersion<V extends { readonly id: string; readonly name: string }>(
  versions: readonly V[],
  defaultVersionId: string | undefined,
): V | undefined {
  if (defaultVersionId !== undefined) {
    const byId = versions.find((version) => version.id === defaultVersionId);
    if (byId !== undefined) return byId;
  }
  return versions.find((version) => version.name === LATEST_VERSION_NAME);
}

/**
 * The numeric id of the version an agent RUNS when nothing pins one, read off
 * a raw agent detail's `versions` and `meta.default_version_id`: the ONE rule
 * (`selectDefaultVersion`: the recorded default, else `base`), then the first
 * version that has a usable id. The desktop host's `default_version_of`
 * (`src-tauri/src/d0/api.rs`) and the Go `GET application` (`detailVersionID`)
 * apply the same order, so the web editor, the web send and the host never
 * run different versions (issue 1941). `is_default` is not read: the server
 * derives it from `meta.default_version_id`.
 *
 * `undefined` when no version has a positive integer id.
 */
export function defaultRunVersionId(
  versions: readonly { readonly id?: unknown; readonly name?: unknown }[],
  defaultVersionId: unknown,
): number | undefined {
  const usable = versions.flatMap((version) => {
    const id = Number(version.id);
    return Number.isSafeInteger(id) && id > 0 ? [{ id: String(id), name: typeof version.name === 'string' ? version.name : '' }] : [];
  });
  const wanted = Number(defaultVersionId);
  const picked =
    selectDefaultVersion(usable, Number.isSafeInteger(wanted) && wanted > 0 ? String(wanted) : undefined) ?? usable[0];
  return picked === undefined ? undefined : Number(picked.id);
}

/**
 * Version-picker ordering (apps/elitea-ui/src/[fsd]/entities/skill-tab-bar/
 * ui/SkillTabBar.jsx:26-33): the default version first, `LATEST_VERSION_NAME`
 * ("base") last, everything else by `createdAt` descending.
 */
export function sortVersionsForPicker(
  versions: readonly VersionSummary[],
  defaultVersionId: string | undefined,
): VersionSummary[] {
  const rank = (version: VersionSummary): number => {
    if (version.id === defaultVersionId) return 0;
    if (version.name === LATEST_VERSION_NAME) return 2;
    return 1;
  };
  return [...versions].sort((a, b) => {
    const rankDiff = rank(a) - rank(b);
    if (rankDiff !== 0) return rankDiff;
    return b.createdAt.localeCompare(a.createdAt);
  });
}

/**
 * apps/elitea-ui/src/[fsd]/entities/version/lib/helpers/version.helpers.jsx:
 * 16-22 `disableSetAsADefault` — a version cannot be (re-)set as default when
 * it already IS the default, when it is the unnamed-default fallback with no
 * explicit `defaultVersionId` set yet, or when it has been published.
 */
export function isSetDefaultDisabled(version: VersionSummary, defaultVersionId: string | undefined): boolean {
  if (version.id === defaultVersionId) return true;
  if (defaultVersionId === undefined && version.name === LATEST_VERSION_NAME) return true;
  if (version.status === 'published') return true;
  return false;
}

/**
 * apps/elitea-ui/src/[fsd]/entities/version/lib/hooks/
 * useIsVersionNotFound.hooks.js:3-11 — string-compared id lookup (ids may
 * arrive from a route param as a string against a numeric-serialized-as-
 * string version id).
 */
export function isVersionNotFound(versionId: string, versions: readonly VersionSummary[]): boolean {
  return !versions.some((version) => String(version.id) === String(versionId));
}
