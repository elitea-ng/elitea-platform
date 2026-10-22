/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/indexing-report/lib/serialize/
 * indexingReport.serialize.js` — turns whatever an index row or history
 * entry actually carries into ONE renderable report shape.
 *
 * **THE DATA IS REAL AND ALREADY REACHABLE, which is why this is worth
 * building.** `GET /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/
 * {toolkitID}` (`services/elitea-main/internal/api/v2/indexing/
 * index_meta.go`) returns `[{id, metadata, stale}]` where `metadata` is
 * `map[string]any` copied verbatim out of the index-registry row
 * (`internal/application/indexmeta/list.go:54-58`) — so the `report`,
 * `skipped`, `indexed`, `total`, `state`, `error` and `history` keys the
 * indexer writes all arrive untouched. `../../ui/IndexDetails/
 * IndexViews.tsx` and `../../ui/IndexesList/IndexListItem.tsx` already read
 * that same map; nothing new is fetched here.
 *
 * Two input shapes, one output:
 *  - a CANONICAL `report` blob (JSON string or object) written by the
 *    current indexer;
 *  - a PRE-REPORT row, where only the raw `IndexingStats` `skipped` blob
 *    survives and the nouns were lost — synthesised into the same shape so
 *    old rows still render a breakdown instead of nothing.
 */
import {
  DEFAULT_INDEXING_DEPENDENT_LABELS,
  DEFAULT_INDEXING_ITEM_LABELS,
  INDEXING_REPORT_KIND_ORDER,
  IndexingReportKind,
  IndexingReportStatus,
  LEGACY_DEPENDENT_GROUPS,
  LEGACY_SKIPPED_GROUPS,
  type IndexingItemLabels,
  type IndexingReportKindValue,
  type IndexingReportStatusValue,
} from '../constants/indexingReport.constants';

const ITEMS_SAMPLE_SIZE = 5;
const FAILED_STATES: readonly string[] = ['failed', 'cancelled'];

export interface IndexingReportGroup {
  readonly reason: string;
  readonly label: string;
  /** The raw worker token of a reason the server has no label for; `''` for a known reason. */
  readonly code: string;
  readonly count: number;
  readonly items: readonly string[];
  /** How many names exist beyond the sampled ones. `0` when no names were recorded at all. */
  readonly more: number;
  readonly dependent: boolean;
  /** Where a category has groups, its count is the sum of the COUNTED ones. */
  readonly counted: boolean;
  readonly itemLabels: IndexingItemLabels | null;
}

export interface IndexingReportCategory {
  readonly kind: IndexingReportKindValue;
  readonly count: number;
  readonly groups: readonly IndexingReportGroup[];
}

export interface IndexingReportTotals {
  readonly indexed: number;
  readonly skipped: number;
  readonly notIndexed: number;
  readonly failed: number;
  readonly unchanged: number;
  readonly dependentNotIndexed: number;
  readonly total: number;
  /** Everything the run left out of the index, however it was left out. */
  readonly leftOut: number;
}

export interface IndexingReport {
  readonly status: IndexingReportStatusValue;
  readonly itemLabels: IndexingItemLabels;
  readonly dependentLabels: IndexingItemLabels;
  readonly totals: IndexingReportTotals;
  readonly categories: readonly IndexingReportCategory[];
  readonly errors: readonly string[];
  readonly errorsTotal: number;
  readonly isUpToDate: boolean;
  readonly isLegacy: boolean;
}

type UnknownRecord = Record<string, unknown>;

function asRecord(value: unknown): UnknownRecord | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as UnknownRecord) : null;
}

/** A `report`/`skipped`/`index_configuration` cell is a JSON string on some rows and an object on others. */
export function parseIndexEntryJson(value: unknown): UnknownRecord | null {
  const direct = asRecord(value);
  if (direct !== null) return direct;
  if (typeof value !== 'string' || value.trim() === '') return null;
  try {
    return asRecord(JSON.parse(value));
  } catch {
    return null;
  }
}

function labelsOf(labels: unknown, fallback: IndexingItemLabels): IndexingItemLabels {
  const record = asRecord(labels);
  const singular = textOf(record?.['singular']);
  const plural = textOf(record?.['plural']);
  return { singular: singular === '' ? fallback.singular : singular, plural: plural === '' ? fallback.plural : plural };
}

/** A server-authored cell is `unknown`; only a real string is text. */
function textOf(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function countOf(value: unknown): number {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

function stringsOf(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : [];
}

export function isUpToDateRun(totals: Partial<IndexingReportTotals> | UnknownRecord | null | undefined): boolean {
  const source = (totals ?? {}) as UnknownRecord;
  return countOf(source['indexed']) === 0 && countOf(source['failed']) === 0 && countOf(source['unchanged']) > 0;
}

interface BuildGroupInput {
  readonly reason: string;
  readonly label: string;
  readonly code?: string;
  readonly count: number;
  readonly items?: readonly string[];
  readonly dependent?: boolean;
  readonly counted?: boolean;
  readonly itemLabels?: IndexingItemLabels | null;
}

function buildGroup(input: BuildGroupInput): IndexingReportGroup {
  const items = input.items ?? [];
  const shown = items.slice(0, ITEMS_SAMPLE_SIZE);
  return {
    reason: input.reason,
    label: input.label,
    code: input.code ?? '',
    count: input.count,
    items: shown,
    // A group whose names were never recorded is fully described by its count.
    more: shown.length > 0 ? Math.max(0, input.count - shown.length) : 0,
    dependent: input.dependent === true,
    counted: input.counted !== false,
    itemLabels: input.itemLabels ?? null,
  };
}

function normalizeCategories(categories: unknown, dependentLabels: IndexingItemLabels): IndexingReportCategory[] {
  const list = Array.isArray(categories) ? categories : [];
  return INDEXING_REPORT_KIND_ORDER.map((kind) => {
    const source = asRecord(list.find((entry) => asRecord(entry)?.['kind'] === kind));
    const groups = Array.isArray(source?.['groups']) ? (source['groups'] as unknown[]) : [];
    return {
      kind,
      count: countOf(source?.['count']),
      groups: groups.map((rawGroup) => {
        const group = asRecord(rawGroup) ?? {};
        const dependent = group['dependent'] === true;
        return buildGroup({
          reason: textOf(group['reason']),
          label: textOf(group['label']),
          code: textOf(group['code']),
          count: countOf(group['count']),
          items: stringsOf(group['items']),
          dependent,
          counted: group['counted'] !== false,
          itemLabels: dependent
            ? labelsOf(group['item_labels'], dependentLabels)
            : (asRecord(group['item_labels']) === null ? null : labelsOf(group['item_labels'], DEFAULT_INDEXING_ITEM_LABELS)),
        });
      }),
    };
  });
}

function normalizeTotals(totals: unknown): IndexingReportTotals {
  const source = asRecord(totals) ?? {};
  const skipped = countOf(source['skipped']);
  const notIndexed = countOf(source['not_indexed']);
  const failed = countOf(source['failed']);
  return {
    indexed: countOf(source['indexed']),
    skipped,
    notIndexed,
    failed,
    unchanged: countOf(source['unchanged']),
    dependentNotIndexed: countOf(source['dependent_not_indexed']),
    total: countOf(source['total']),
    leftOut: skipped + notIndexed + failed,
  };
}

/**
 * A failed run whose report says everything went fine is a report left over
 * from an EARLIER run. Trusting it would show last run's success on this
 * run's failure.
 */
function contradictsState(report: UnknownRecord, state: unknown): boolean {
  return typeof state === 'string' && FAILED_STATES.includes(state) && report['status'] === IndexingReportStatus.ok;
}

function fromCanonicalReport(report: UnknownRecord): IndexingReport {
  const itemLabels = labelsOf(report['item_labels'], DEFAULT_INDEXING_ITEM_LABELS);
  const dependentLabels = labelsOf(report['dependent_labels'], DEFAULT_INDEXING_DEPENDENT_LABELS);
  const totals = normalizeTotals(report['totals']);

  const status = report['status'];
  return {
    status: typeof status === 'string' ? (status as IndexingReportStatusValue) : IndexingReportStatus.ok,
    itemLabels,
    dependentLabels,
    // A failed or stopped run's counts are the report's own: the row's
    // `indexed` is what the index holds, not what this run did.
    totals,
    categories: normalizeCategories(report['categories'], dependentLabels),
    errors: stringsOf(report['errors']),
    errorsTotal: countOf(report['errors_total']),
    isUpToDate: isUpToDateRun(asRecord(report['totals'])),
    isLegacy: false,
  };
}

interface MergedLegacyGroup {
  reason: string;
  label: string;
  count: number;
  names: Set<string>;
  duplicates: number;
}

function legacyGroupsFor(
  kind: IndexingReportKindValue,
  skipped: UnknownRecord | null,
  dependentLabels: IndexingItemLabels,
): IndexingReportGroup[] {
  const merged = new Map<string, MergedLegacyGroup>();
  LEGACY_SKIPPED_GROUPS.filter((spec) => spec.kind === kind).forEach((spec) => {
    const section = asRecord(skipped?.[spec.section]) ?? {};
    const count = countOf(section[spec.countKey]);
    const items = stringsOf(section[spec.itemsKey]);
    const existing = merged.get(spec.reason);
    if (existing === undefined) {
      merged.set(spec.reason, { reason: spec.reason, label: spec.label, count, names: new Set(items), duplicates: 0 });
      return;
    }
    // The SDK unions the underlying sets, so a name in both is ONE item there.
    items.forEach((item) => {
      if (existing.names.has(item)) existing.duplicates += 1;
      existing.names.add(item);
    });
    existing.count += count;
  });

  const groups = [...merged.values()].map((group) =>
    buildGroup({
      reason: group.reason,
      label: group.label,
      count: Math.max(0, group.count - group.duplicates),
      items: [...group.names],
    }),
  );

  const dependentGroups = LEGACY_DEPENDENT_GROUPS.filter((spec) => spec.kind === kind).map((spec) => {
    const section = asRecord(skipped?.[spec.section]) ?? {};
    return buildGroup({
      reason: spec.reason,
      label: spec.label,
      count: countOf(section['count']),
      items: stringsOf(section['items']),
      dependent: true,
      counted: false,
      itemLabels: dependentLabels,
    });
  });

  return [...groups, ...dependentGroups].filter((group) => group.count > 0);
}

/** Pre-report rows still carry the raw `IndexingStats` blob; only the nouns were lost. */
function fromLegacyEntry(entry: UnknownRecord): IndexingReport {
  const skipped = parseIndexEntryJson(entry['skipped']);
  const dependentLabels = DEFAULT_INDEXING_DEPENDENT_LABELS;
  const unchanged = countOf(asRecord(skipped?.['documents_already_indexed'])?.['count']);

  // The persisted `indexed` counts everything in the store, unchanged items
  // included; the breakdown is about THIS run, so unchanged items get their
  // own line.
  const persistedIndexed = countOf(entry['indexed']);
  const runIndexed = skipped !== null ? Math.max(0, persistedIndexed - unchanged) : persistedIndexed;

  const categories: IndexingReportCategory[] = INDEXING_REPORT_KIND_ORDER.map((kind) => {
    if (kind === IndexingReportKind.indexed) return { kind, count: runIndexed, groups: [] };
    const groups = legacyGroupsFor(kind, skipped, dependentLabels);
    if (kind === IndexingReportKind.skipped && unchanged > 0) {
      groups.unshift(
        buildGroup({
          reason: 'unchanged',
          label: 'Already indexed (unchanged)',
          count: unchanged,
          items: stringsOf(asRecord(skipped?.['documents_already_indexed'])?.['items']),
          counted: false,
        }),
      );
    }
    return {
      kind,
      count: groups.filter((group) => group.counted).reduce((sum, group) => sum + group.count, 0),
      groups,
    };
  });

  const countOfKind = (kind: IndexingReportKindValue): number =>
    categories.find((category) => category.kind === kind)?.count ?? 0;
  const skippedCount = countOfKind(IndexingReportKind.skipped);
  const notIndexedCount = countOfKind(IndexingReportKind.notIndexed);
  const failedCount = countOfKind(IndexingReportKind.failed);

  const totals: IndexingReportTotals = {
    indexed: runIndexed,
    skipped: skippedCount,
    notIndexed: notIndexedCount,
    failed: failedCount,
    unchanged,
    dependentNotIndexed: categories.reduce(
      (sum, category) =>
        sum + category.groups.filter((group) => group.dependent).reduce((inner, group) => inner + group.count, 0),
      0,
    ),
    // `persistedIndexed` already counts unchanged items, so it stands in for
    // indexed + unchanged in the totals contract.
    total: countOf(entry['total']) || persistedIndexed + skippedCount + notIndexedCount + failedCount,
    leftOut: skippedCount + notIndexedCount + failedCount,
  };

  const error = textOf(entry['error']).trim();
  const state = entry['state'];
  const isFailed = (typeof state === 'string' && FAILED_STATES.includes(state)) || error !== '';

  return {
    status: isFailed ? IndexingReportStatus.error : IndexingReportStatus.ok,
    itemLabels: DEFAULT_INDEXING_ITEM_LABELS,
    dependentLabels,
    totals,
    categories,
    errors: error !== '' ? [error] : [],
    errorsTotal: error !== '' ? 1 : 0,
    isUpToDate: !isFailed && isUpToDateRun(totals),
    isLegacy: true,
  };
}

/**
 * Build a renderable report from any surface that carries one: an
 * `index_meta` row, a `history` entry, or a parsed tool result. Falls back
 * to synthesising one from the pre-report fields so old records still render
 * a breakdown. Returns `null` when the source carries no run information at
 * all — callers render nothing rather than an empty shell.
 */
export function normalizeIndexingReport(source: unknown): IndexingReport | null {
  const root = asRecord(source);
  if (root === null) return null;

  const entry = asRecord(root['metadata']) ?? root;
  const report = parseIndexEntryJson(entry['report']);

  if (report !== null && !contradictsState(report, entry['state'])) return fromCanonicalReport(report);
  if (entry['skipped'] !== undefined || entry['indexed'] !== undefined || entry['state'] !== undefined || entry['error'] !== undefined) {
    return fromLegacyEntry(entry);
  }
  return null;
}

/**
 * Accept either an already-normalised report or any raw surface that carries
 * one, so callers do not each have to know how to tell them apart.
 */
export function resolveIndexingReport(source: unknown): IndexingReport | null {
  const root = asRecord(source);
  if (root !== null && Array.isArray(root['categories'])) return root as unknown as IndexingReport;
  return normalizeIndexingReport(source);
}
