import { describe, expect, it } from 'vitest';

import { IndexingReportKind, IndexingReportStatus } from '../constants/indexingReport.constants';
import {
  isUpToDateRun,
  normalizeIndexingReport,
  parseIndexEntryJson,
  resolveIndexingReport,
} from './indexingReport.serialize';

const canonical = {
  status: 'partly_indexed',
  item_labels: { singular: 'page', plural: 'pages' },
  totals: { indexed: 4, skipped: 2, not_indexed: 1, failed: 1, unchanged: 3, total: 11 },
  categories: [
    {
      kind: 'skipped',
      count: 2,
      groups: [{ reason: 'filtered', label: 'Excluded by configured filters', count: 2, items: ['a.md', 'b.md'] }],
    },
  ],
  errors: ['boom'],
  errors_total: 3,
};

describe('parseIndexEntryJson', () => {
  it('accepts an object, a JSON string, and rejects everything else', () => {
    expect(parseIndexEntryJson({ a: 1 })).toEqual({ a: 1 });
    expect(parseIndexEntryJson('{"a":1}')).toEqual({ a: 1 });
    expect(parseIndexEntryJson('not json')).toBeNull();
    expect(parseIndexEntryJson('  ')).toBeNull();
    expect(parseIndexEntryJson('[1,2]')).toBeNull();
    expect(parseIndexEntryJson(null)).toBeNull();
  });
});

describe('normalizeIndexingReport — canonical report', () => {
  it('reads totals, labels, categories and errors off the report blob', () => {
    const report = normalizeIndexingReport({ report: canonical });
    expect(report?.status).toBe(IndexingReportStatus.partlyIndexed);
    expect(report?.itemLabels).toEqual({ singular: 'page', plural: 'pages' });
    expect(report?.totals.leftOut).toBe(4);
    expect(report?.errorsTotal).toBe(3);
    expect(report?.isLegacy).toBe(false);
  });

  it('parses a report delivered as a JSON string, as the wire sometimes does', () => {
    expect(normalizeIndexingReport({ report: JSON.stringify(canonical) })?.totals.indexed).toBe(4);
  });

  it('orders categories canonically and samples at most five names per group', () => {
    const many = Array.from({ length: 9 }, (_, i) => `f${String(i)}.md`);
    const report = normalizeIndexingReport({
      report: { ...canonical, categories: [{ kind: 'skipped', count: 9, groups: [{ reason: 'filtered', label: 'L', count: 9, items: many }] }] },
    });
    expect(report?.categories.map((category) => category.kind)).toEqual([
      IndexingReportKind.indexed,
      IndexingReportKind.skipped,
      IndexingReportKind.notIndexed,
      IndexingReportKind.failed,
    ]);
    const group = report?.categories[1]?.groups[0];
    expect(group?.items).toHaveLength(5);
    expect(group?.more).toBe(4);
  });

  it('reports `more: 0` for a group whose names were never recorded — its count already says everything', () => {
    const report = normalizeIndexingReport({
      report: { ...canonical, categories: [{ kind: 'failed', count: 7, groups: [{ reason: 'read_error', label: 'L', count: 7 }] }] },
    });
    expect(report?.categories[3]?.groups[0]?.more).toBe(0);
  });

  /** The regression the baseline calls out by name: a stale success report on a failed run. */
  it('ignores an ok report that contradicts a failed/cancelled state and falls back to the raw fields', () => {
    const report = normalizeIndexingReport({
      state: 'failed',
      indexed: 2,
      error: 'the source went away',
      report: { status: 'ok', totals: { indexed: 99 } },
    });
    expect(report?.isLegacy).toBe(true);
    expect(report?.status).toBe(IndexingReportStatus.error);
    expect(report?.errors).toEqual(['the source went away']);
    expect(report?.totals.indexed).toBe(2);
  });

  it('takes a failed run\'s counts from its own report, not from the row, which holds the index total', () => {
    const report = normalizeIndexingReport({
      indexed: 6,
      total: 12,
      report: { status: 'error', totals: { indexed: 0, total: 0 } },
    });
    expect(report?.totals.indexed).toBe(0);
    expect(report?.totals.total).toBe(0);
  });
});

describe('normalizeIndexingReport — pre-report rows', () => {
  const skipped = {
    documents_already_indexed: { count: 3, items: ['old-1'] },
    documents_skipped: { filtered_count: 2, filtered: ['x.md', 'y.md'], error_count: 1, error: ['z.md'] },
    files_skipped: { unsupported_extension_count: 1, unsupported_extension: ['a.bin'] },
    dependent_items_filtered: { count: 4, items: ['att.png'] },
  };

  it('synthesises categories from the raw IndexingStats blob', () => {
    const report = normalizeIndexingReport({ indexed: 10, skipped, state: 'completed' });
    expect(report?.isLegacy).toBe(true);
    // 10 persisted - 3 unchanged = 7 indexed by THIS run.
    expect(report?.totals.indexed).toBe(7);
    expect(report?.totals.unchanged).toBe(3);
    expect(report?.totals.skipped).toBe(2);
    expect(report?.totals.notIndexed).toBe(1);
    expect(report?.totals.failed).toBe(1);
    expect(report?.totals.leftOut).toBe(4);
  });

  it('keeps the unchanged group present but uncounted, so it is not double-reported as skipped', () => {
    const report = normalizeIndexingReport({ indexed: 10, skipped });
    const skippedCategory = report?.categories.find((category) => category.kind === IndexingReportKind.skipped);
    const unchangedGroup = skippedCategory?.groups.find((group) => group.reason === 'unchanged');
    expect(unchangedGroup?.counted).toBe(false);
    expect(unchangedGroup?.count).toBe(3);
    expect(skippedCategory?.count).toBe(2);
  });

  it('does not count dependent groups toward the category total', () => {
    const report = normalizeIndexingReport({ indexed: 1, skipped });
    expect(report?.totals.dependentNotIndexed).toBe(4);
  });

  /** The SDK unions the underlying name sets, so a name in two sections is ONE item there. */
  it('deduplicates names merged from two sections of the same reason', () => {
    const report = normalizeIndexingReport({
      indexed: 0,
      skipped: {
        documents_skipped: { error_count: 1, error: ['same.md'] },
        runtime_skipped: { error_count: 1, error: ['same.md'] },
      },
    });
    const failed = report?.categories.find((category) => category.kind === IndexingReportKind.failed);
    expect(failed?.count).toBe(1);
    expect(failed?.groups[0]?.items).toEqual(['same.md']);
  });

  it('treats a stored error as a failure even when the state says otherwise', () => {
    expect(normalizeIndexingReport({ indexed: 1, error: '  ' })?.status).toBe(IndexingReportStatus.ok);
    expect(normalizeIndexingReport({ indexed: 1, error: 'nope' })?.status).toBe(IndexingReportStatus.error);
  });

  it('unwraps a `metadata` envelope, which is the shape the index_meta rows arrive in', () => {
    expect(normalizeIndexingReport({ id: '1', metadata: { indexed: 4 } })?.totals.indexed).toBe(4);
  });

  it('returns null for a source with no run information at all', () => {
    expect(normalizeIndexingReport(null)).toBeNull();
    expect(normalizeIndexingReport('nope')).toBeNull();
    expect(normalizeIndexingReport({ collection: 'my-index' })).toBeNull();
  });
});

describe('isUpToDateRun / resolveIndexingReport', () => {
  it('calls a run up to date only when nothing moved and something was unchanged', () => {
    expect(isUpToDateRun({ indexed: 0, failed: 0, unchanged: 5 })).toBe(true);
    expect(isUpToDateRun({ indexed: 1, failed: 0, unchanged: 5 })).toBe(false);
    expect(isUpToDateRun({ indexed: 0, failed: 0, unchanged: 0 })).toBe(false);
    expect(isUpToDateRun(null)).toBe(false);
  });

  it('passes an already-normalised report straight through', () => {
    const normalised = normalizeIndexingReport({ report: canonical });
    expect(resolveIndexingReport(normalised)).toBe(normalised);
    expect(resolveIndexingReport({ report: canonical })?.totals.indexed).toBe(4);
  });
});
