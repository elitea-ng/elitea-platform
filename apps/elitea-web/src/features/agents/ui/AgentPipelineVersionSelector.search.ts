/**
 * Issue 940/A11 (ELITEA-3278/3280/3281) — the version-selector dropdown's
 * search box: pure filter/format/display helpers, split out of
 * `AgentPipelineVersionSelector.tsx` purely to keep that file (and its
 * sibling `AgentPipelineVersionSelector.menu.tsx`) under the §3.5 400-line
 * budget, the same split `AgentVersionMenuCommands.tsx`'s own module doc
 * already documents for the identical reason.
 *
 * `DisplayVersion`/`toDisplayVersions`/`formatVersionDisplayText` (pre-A11
 * residents of the main component file) live HERE rather than in either
 * `.tsx` sibling specifically so neither of those two ever needs to import
 * FROM the other — `AgentPipelineVersionSelector.tsx` builds `DisplayVersion`
 * rows and renders the trigger; `AgentPipelineVersionSelector.menu.tsx`
 * renders the dropdown body off the same rows. Both depend on this leaf
 * module; this module depends on neither.
 */
import type { AgentPipelineVersionOption } from '../lib/types';

/** `apps/elitea-ui/src/pages/Applications/Components/Tools/AgentPipelineVersionSelector.jsx`'s own literal — the version whose `name` marks it as the always-latest/unnamed one. */
export const LATEST_VERSION_NAME = 'base';

export interface DisplayVersion extends AgentPipelineVersionOption {
  readonly isLatest: boolean;
}

export function toDisplayVersions(versions: readonly AgentPipelineVersionOption[]): readonly DisplayVersion[] {
  return [...versions]
    .map((version): DisplayVersion => ({ ...version, isLatest: version.name === LATEST_VERSION_NAME }))
    .sort((a, b) => {
      if (a.isLatest && !b.isLatest) return -1;
      if (!a.isLatest && b.isLatest) return 1;
      const dateA = new Date(a.created_at ?? 0).getTime();
      const dateB = new Date(b.created_at ?? 0).getTime();
      return dateB - dateA;
    });
}

/** The trigger's and each row's PRIMARY label — compact "name – DD.MM.YYYY" (day.month.year, no time). Distinct from `formatVersionTimestamp` below, the row's secondary line (ELITEA-3279's full "MMM DD, YYYY, hh:mm AM/PM"). */
export function formatVersionDisplayText(version: DisplayVersion): string {
  if (version.isLatest) return LATEST_VERSION_NAME;
  const versionName = version.name || 'Unnamed version';
  if (!version.created_at) return versionName;
  const date = new Date(version.created_at);
  if (Number.isNaN(date.getTime())) return versionName;
  const day = String(date.getDate()).padStart(2, '0');
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const year = date.getFullYear();
  return `${versionName} – ${day}.${month}.${year}`;
}

/** Duck-typed rather than declared as `DisplayVersion` directly: `filterVersionsBySearch` only ever needs these 4 fields, and staying structural keeps this usable from a bare `AgentPipelineVersionOption` too, should a future caller ever want to search before `toDisplayVersions` runs. */
export interface SearchableVersion {
  readonly name: string;
  readonly isLatest: boolean;
  readonly created_at?: string | undefined;
  readonly author?: { readonly name?: string | undefined; readonly email?: string | undefined } | undefined;
}

/**
 * The version's search-matchable NAME — `'base'` for the latest/unnamed
 * version (the literal word the trigger and every row already render for
 * it), not the possibly-empty `name` field underneath.
 */
export function searchableVersionName(version: SearchableVersion): string {
  return version.isLatest ? LATEST_VERSION_NAME : version.name || '';
}

/**
 * ELITEA-3278 — filters by name (case-insensitive, partial match, `'base'`
 * included) OR by creator (name AND email, same match rules), preserving
 * the caller's own order (ELITEA-3280: the caller sorts by timestamp
 * BEFORE calling this, and `.filter()` never reorders).
 */
export function filterVersionsBySearch<T extends SearchableVersion>(versions: readonly T[], query: string): readonly T[] {
  const trimmed = query.trim().toLowerCase();
  if (trimmed === '') return versions;
  return versions.filter((version) => {
    const name = searchableVersionName(version).toLowerCase();
    const creatorName = version.author?.name?.toLowerCase() ?? '';
    const creatorEmail = version.author?.email?.toLowerCase() ?? '';
    return name.includes(trimmed) || creatorName.includes(trimmed) || creatorEmail.includes(trimmed);
  });
}

/**
 * ELITEA-3279's own required shape: "MMM DD, YYYY, hh:mm AM/PM" (e.g. "Sep
 * 10, 2026, 3:45 PM"). Distinct from `formatVersionDisplayText`'s compact
 * "DD.MM.YYYY" (day.month.year, no time) — that format stays exactly as it
 * is on the trigger and the row's own primary line; this is the row's
 * SECONDARY line only (see the component's `renderMenu`).
 */
export function formatVersionTimestamp(createdAt: string | undefined): string | undefined {
  if (!createdAt) return undefined;
  const date = new Date(createdAt);
  if (Number.isNaN(date.getTime())) return undefined;
  return new Intl.DateTimeFormat('en-US', {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    hour12: true,
  }).format(date);
}

/** The creator label the row's secondary line names — the name when the join resolved one, the email otherwise, absent when neither is known. */
export function versionCreatorLabel(version: SearchableVersion): string | undefined {
  const name = version.author?.name;
  if (name) return name;
  const email = version.author?.email;
  return email || undefined;
}

/** ELITEA-3279 — the row's secondary "creator · timestamp" line, or `undefined` when there is nothing to show (the `base` row, or a version with neither field known). */
export function versionMetaLine(version: SearchableVersion): string | undefined {
  const parts = [versionCreatorLabel(version), formatVersionTimestamp(version.created_at)].filter((part): part is string => part !== undefined);
  return parts.length > 0 ? parts.join(' · ') : undefined;
}
