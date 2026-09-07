/**
 * Joining a configured source to the status the provider reports for it.
 *
 * THE TWO SIDES DO NOT SHARE A KEY, and this is the whole difficulty of the
 * sources screen. The toolkit's settings name a source by its TOOLKIT ID —
 * `sources: [9010]`. The provider reports a status under whatever label the
 * ingestion ran with, and there are three of those in circulation:
 *
 *   `github:9010`  the expanded source object's `{type}:{id}`, which is what
 *                  `SourceLabelFor` builds from the body the facade rewrote
 *                  (internal/apps/inventory/run/fixture.go:149-172).
 *   `9010`         the bare id, from an ingestion that ran before the facade
 *                  expanded anything.
 *   `code`         the SDK toolkit's own short name, which is what the graph's
 *                  CITATIONS carry — so `list_ingested_sources` and the
 *                  entity rows report that one.
 *
 * A screen that matched only the first would show every source as never
 * ingested on a graph full of entities, and there is nothing on that screen to
 * tell the user that the join, rather than the ingestion, is what failed. So
 * all three are matched, most specific first, and the match is a pure function
 * with its own tests rather than an expression inside a component.
 */

/** What is known about one configured source before its status is joined. */
export interface SourceIdentity {
  readonly toolkitId: string;
  readonly name: string;
  readonly type: string;
}

/** Anything that reports a status under a label. */
export interface LabelledStatus {
  readonly source: string;
}

function normalise(value: string): string {
  return value.trim().toLowerCase();
}

/**
 * Whether one reported label names one configured source.
 *
 * `{type}:{id}` is matched on the ID HALF only. Matching the whole label
 * against `type` and `id` separately would make `github:9010` match a source
 * of type github with a different id — every github source in the project
 * would share one status.
 */
export function labelNamesSource(label: string, source: SourceIdentity): boolean {
  const candidate = normalise(label);
  if (candidate === '') return false;
  const id = normalise(source.toolkitId);
  if (candidate === id) return true;

  const separator = candidate.lastIndexOf(':');
  if (separator >= 0 && candidate.slice(separator + 1) === id) return true;

  // The citation name. Checked LAST, and only when it is not empty: an
  // unnamed source would otherwise match every unnamed label.
  const name = normalise(source.name);
  if (name !== '' && candidate === name) return true;
  return false;
}

/**
 * The status reported for one source, or `undefined`.
 *
 * The FIRST match wins and the list is scanned in the provider's own order,
 * which is the order the ingestions ran. A source ingested twice has one row —
 * the provider rewrites it — so a second match would be a provider defect, and
 * silently preferring the last one would hide it.
 */
export function statusForSource<T extends LabelledStatus>(
  statuses: readonly T[],
  source: SourceIdentity,
): T | undefined {
  return statuses.find((status) => labelNamesSource(status.source, source));
}
