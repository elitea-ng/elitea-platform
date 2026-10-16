/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/indexing-report/lib/constants/
 * indexingReport.constants.js`.
 *
 * **WHY THIS LIVES UNDER `features/toolkits/indexes` AND NOT IN A NEW
 * `entities/indexing-report` SLICE.** The baseline keeps the report in an
 * entities-layer slice because four unrelated surfaces read it (index list,
 * run-history detail, run-index form, notification rows). In this app only
 * one of those surfaces exists — the index details screen this directory
 * owns — and `entities/run-history`, the sibling slice the baseline's own
 * report consumers hang off, was already decided against for exactly this
 * reason (see `../../api/indexesApi.ts` and `../../ui/IndexDetails/
 * IndexHistory.tsx`, which inline their run-history dependencies for the
 * same "no promoted slice yet" rationale). Creating an entities slice with
 * a single features-layer consumer would be dead generality; the promotion
 * belongs to whichever later unit brings a SECOND consumer.
 */

export const IndexingReportKind = {
  indexed: 'indexed',
  skipped: 'skipped',
  notIndexed: 'not_indexed',
  failed: 'failed',
} as const;

export type IndexingReportKindValue = (typeof IndexingReportKind)[keyof typeof IndexingReportKind];

export const IndexingReportStatus = {
  ok: 'ok',
  partlyIndexed: 'partly_indexed',
  error: 'error',
  /** Written by the Rust indexing runtime for a run that was stopped. */
  cancelled: 'cancelled',
} as const;

export type IndexingReportStatusValue = (typeof IndexingReportStatus)[keyof typeof IndexingReportStatus];

export const INDEXING_REPORT_KIND_ORDER: readonly IndexingReportKindValue[] = [
  IndexingReportKind.indexed,
  IndexingReportKind.skipped,
  IndexingReportKind.notIndexed,
  IndexingReportKind.failed,
];

/** MUI palette key a category's headline is tinted with. */
type IndexingReportTone = 'success' | 'warning' | 'error';

export interface IndexingReportKindPresentation {
  readonly icon: string;
  readonly verb: string;
  readonly tone: IndexingReportTone;
}

export const INDEXING_REPORT_KIND_PRESENTATION: Readonly<
  Record<IndexingReportKindValue, IndexingReportKindPresentation>
> = {
  [IndexingReportKind.indexed]: { icon: '✅', verb: 'indexed', tone: 'success' },
  [IndexingReportKind.skipped]: { icon: '⚠️', verb: 'skipped', tone: 'warning' },
  [IndexingReportKind.notIndexed]: { icon: '⚠️', verb: 'not indexed', tone: 'warning' },
  [IndexingReportKind.failed]: { icon: '❌', verb: 'failed', tone: 'error' },
};

export interface IndexingItemLabels {
  readonly singular: string;
  readonly plural: string;
}

export const DEFAULT_INDEXING_ITEM_LABELS: IndexingItemLabels = { singular: 'document', plural: 'documents' };
export const DEFAULT_INDEXING_DEPENDENT_LABELS: IndexingItemLabels = { singular: 'attachment', plural: 'attachments' };

/** One `skipped`-blob section mapped onto a report group. */
export interface LegacySkippedGroupSpec {
  readonly kind: IndexingReportKindValue;
  readonly reason: string;
  readonly label: string;
  readonly section: string;
  readonly countKey: string;
  readonly itemsKey: string;
}

/** Maps the pre-report `skipped` blob (`IndexingStats.to_dict`) onto report groups. */
export const LEGACY_SKIPPED_GROUPS: readonly LegacySkippedGroupSpec[] = [
  {
    kind: IndexingReportKind.skipped,
    reason: 'filtered',
    label: 'Excluded by configured filters',
    section: 'documents_skipped',
    countKey: 'filtered_count',
    itemsKey: 'filtered',
  },
  {
    kind: IndexingReportKind.skipped,
    reason: 'not_in_whitelist',
    label: 'Not matching the configured include patterns',
    section: 'files_skipped',
    countKey: 'whitelist_filtered_count',
    itemsKey: 'whitelist_filtered',
  },
  {
    kind: IndexingReportKind.skipped,
    reason: 'blacklisted',
    label: 'Matching the configured exclude patterns',
    section: 'files_skipped',
    countKey: 'blacklist_filtered_count',
    itemsKey: 'blacklist_filtered',
  },
  {
    kind: IndexingReportKind.skipped,
    reason: 'empty',
    label: 'Contained no indexable content',
    section: 'files_skipped',
    countKey: 'empty_content_count',
    itemsKey: 'empty_content',
  },
  {
    kind: IndexingReportKind.notIndexed,
    reason: 'unsupported_format',
    label: 'Unsupported format',
    section: 'files_skipped',
    countKey: 'unsupported_extension_count',
    itemsKey: 'unsupported_extension',
  },
  {
    kind: IndexingReportKind.notIndexed,
    reason: 'unsupported_format',
    label: 'Unsupported format',
    section: 'runtime_skipped',
    countKey: 'extension_filtered_count',
    itemsKey: 'extension_filtered',
  },
  {
    kind: IndexingReportKind.failed,
    reason: 'read_error',
    label: 'Could not be read',
    section: 'files_skipped',
    countKey: 'read_error_count',
    itemsKey: 'read_error',
  },
  {
    kind: IndexingReportKind.failed,
    reason: 'processing_error',
    label: 'Could not be processed',
    section: 'documents_skipped',
    countKey: 'error_count',
    itemsKey: 'error',
  },
  {
    kind: IndexingReportKind.failed,
    reason: 'processing_error',
    label: 'Could not be processed',
    section: 'runtime_skipped',
    countKey: 'error_count',
    itemsKey: 'error',
  },
];

export interface LegacyDependentGroupSpec {
  readonly kind: IndexingReportKindValue;
  readonly reason: string;
  readonly label: string;
  readonly section: string;
}

export const LEGACY_DEPENDENT_GROUPS: readonly LegacyDependentGroupSpec[] = [
  {
    kind: IndexingReportKind.skipped,
    reason: 'filtered',
    label: 'Excluded by configured filters',
    section: 'dependent_items_filtered',
  },
  {
    kind: IndexingReportKind.notIndexed,
    reason: 'unsupported_format',
    label: 'Unsupported format',
    section: 'dependent_items_unsupported',
  },
  {
    kind: IndexingReportKind.skipped,
    reason: 'empty',
    label: 'Contained no indexable content',
    section: 'dependent_items_empty',
  },
  {
    kind: IndexingReportKind.failed,
    reason: 'processing_error',
    label: 'Could not be processed',
    section: 'dependent_items_skipped',
  },
];
