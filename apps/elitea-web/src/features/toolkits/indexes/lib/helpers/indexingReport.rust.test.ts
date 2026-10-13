import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

import { IndexingReportKind } from '../constants/indexingReport.constants';
import { normalizeIndexingReport } from './indexingReport.serialize';

// The report elitea-main writes for a Rust indexing run, pinned by the Go
// golden test (indexregistry.TestMetadataReportGolden).
const golden = JSON.parse(
  readFileSync(
    resolve(
      __dirname,
      '../../../../../../../../services/elitea-main/internal/application/indexregistry/testdata/rust_run_report.golden.json',
    ),
    'utf8',
  ),
) as unknown;

// A Rust-shaped index_meta row: the report plus the flat `skipped` map the
// registry keeps for compatibility, which the legacy parse cannot read.
const rustRow = {
  metadata: {
    state: 'partly_indexed',
    indexed: 8,
    total: 21,
    skipped: { empty: 1, fetch_failed: 1, too_large: 2, unreadable: 2, unsupported_type: 4, weird_new_reason: 3 },
    report: golden,
  },
};

describe('normalizeIndexingReport on a Rust run', () => {
  it('renders the breakdown from the canonical report, not the flat skipped map', () => {
    const report = normalizeIndexingReport(rustRow);
    expect(report?.isLegacy).toBe(false);
    expect(report?.totals).toMatchObject({ indexed: 3, skipped: 1, notIndexed: 9, failed: 3, unchanged: 5, total: 21 });

    const byKind = (kind: string) => report?.categories.find((category) => category.kind === kind);
    expect(byKind(IndexingReportKind.notIndexed)?.groups.map((group) => group.reason)).toEqual([
      'too_large',
      'unsupported_format',
      'weird_new_reason',
    ]);
    expect(byKind(IndexingReportKind.failed)?.groups).toMatchObject([{ reason: 'read_error', count: 3 }]);
  });

  it('shows the unchanged line', () => {
    const skipped = normalizeIndexingReport(rustRow)?.categories.find((c) => c.kind === IndexingReportKind.skipped);
    expect(skipped?.groups.find((group) => group.reason === 'unchanged')).toMatchObject({
      label: 'Already indexed (unchanged)',
      count: 5,
      counted: false,
    });
    // Unchanged documents are context, not part of the category count.
    expect(skipped?.count).toBe(1);
  });

  it('does not treat a run that wrote documents as up to date', () => {
    expect(normalizeIndexingReport(rustRow)?.isUpToDate).toBe(false);
  });
});

describe('normalizeIndexingReport on a stopped or failed Rust run', () => {
  const stopped = {
    metadata: {
      state: 'cancelled',
      indexed: 2,
      total: 2,
      index_total_documents: 30,
      report: {
        version: 1,
        status: 'cancelled',
        totals: { indexed: 2, skipped: 0, not_indexed: 0, failed: 0, unchanged: 0, dependent_not_indexed: 0, total: 2 },
        categories: [],
        errors: ['Indexing was stopped before it finished.'],
        errors_total: 1,
      },
    },
  };

  it('keeps the run-facing counts and the stop message', () => {
    const report = normalizeIndexingReport(stopped);
    expect(report?.status).toBe('cancelled');
    expect(report?.totals).toMatchObject({ indexed: 2, total: 2, unchanged: 0 });
    expect(report?.errors).toEqual(['Indexing was stopped before it finished.']);
  });

  it('labels an unknown skip reason generically and keeps the token as code', () => {
    const report = normalizeIndexingReport(rustRow);
    const group = report?.categories
      .find((category) => category.kind === IndexingReportKind.notIndexed)
      ?.groups.find((g) => g.reason === 'weird_new_reason');
    expect(group?.label).toBe('Not indexed (other)');
  });
});
