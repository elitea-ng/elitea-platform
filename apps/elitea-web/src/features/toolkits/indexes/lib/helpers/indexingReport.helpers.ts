/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/indexing-report/lib/helpers/
 * indexingReport.helpers.js` — the wording layer over
 * `./indexingReport.serialize.ts`'s normalised shape.
 *
 * NOT ported: `formatIndexingReportText` and `summarizeIndexingReport`. Both
 * exist upstream for surfaces this app does not have yet (a chat-content
 * renderer for toolkit conversations, and notification rows). Declaring them
 * here with no caller is precisely what `scripts/check-dead-code.mjs` and
 * knip reject; whichever unit brings one of those surfaces should add the
 * one it needs.
 */
import {
  INDEXING_REPORT_KIND_PRESENTATION,
  IndexingReportStatus,
  type IndexingItemLabels,
} from '../constants/indexingReport.constants';
import type { IndexingReport, IndexingReportCategory, IndexingReportGroup } from './indexingReport.serialize';

export function pickItemNoun(count: number, labels: IndexingItemLabels): string {
  return count === 1 ? labels.singular : labels.plural;
}

/**
 * Groups worth showing for a category. Unchanged items are the whole story
 * of an up-to-date run and are already in its headline, so repeating them as
 * "skipped" there would contradict it.
 */
function visibleCategoryGroups(category: IndexingReportCategory): IndexingReportGroup[] {
  return category.groups.filter((group) => group.count > 0 && group.reason !== 'unchanged');
}

/** Categories with something to say — the count they carry, or a dependent group. */
export function visibleCategories(report: IndexingReport): IndexingReportCategory[] {
  return report.categories
    .map((category) => ({ ...category, groups: visibleCategoryGroups(category) }))
    .filter((category) => category.count > 0 || category.groups.some((group) => group.dependent));
}

export interface IndexingReportHeadline {
  readonly icon: string;
  readonly text: string;
}

export function categoryHeadline(category: IndexingReportCategory, report: IndexingReport): IndexingReportHeadline {
  const { icon, verb } = INDEXING_REPORT_KIND_PRESENTATION[category.kind];
  if (category.count > 0) {
    return { icon, text: `${String(category.count)} ${pickItemNoun(category.count, report.itemLabels)} ${verb}` };
  }
  const dependentCount = category.groups
    .filter((group) => group.dependent)
    .reduce((sum, group) => sum + group.count, 0);
  const labels = category.groups.find((group) => group.dependent)?.itemLabels ?? report.dependentLabels;
  return { icon, text: `${String(dependentCount)} ${pickItemNoun(dependentCount, labels)} ${verb}` };
}

export function reportHeadline(report: IndexingReport): IndexingReportHeadline | null {
  const { totals, itemLabels } = report;
  if (report.status === IndexingReportStatus.error && totals.indexed === 0) {
    return { icon: '❌', text: `Failed to index ${itemLabels.plural}` };
  }
  if (report.status === IndexingReportStatus.cancelled && totals.indexed === 0) {
    return { icon: '⏹️', text: `Stopped before any ${itemLabels.plural} were indexed` };
  }
  if (report.isUpToDate) {
    const { unchanged } = totals;
    return { icon: '✅', text: `Up to date — ${String(unchanged)} ${pickItemNoun(unchanged, itemLabels)} unchanged` };
  }
  if (totals.indexed === 0 && totals.skipped === 0 && totals.notIndexed === 0 && totals.failed === 0) {
    return { icon: 'ℹ️', text: 'No documents to index' };
  }
  return null;
}

export interface IndexingUnchangedNotice {
  readonly count: number;
  readonly text: string;
}

/**
 * The unchanged tally, when it still needs saying. Unchanged items are
 * stripped from the skipped category by `visibleCategories`, so every
 * renderer has to put them back — this keeps that decision and its wording
 * in one place.
 */
export function unchangedNotice(report: IndexingReport | null | undefined): IndexingUnchangedNotice | null {
  const count = report?.totals.unchanged ?? 0;
  if (count === 0 || report === null || report === undefined || report.isUpToDate) return null;
  return { count, text: `${String(count)} ${pickItemNoun(count, report.itemLabels)} already indexed (unchanged)` };
}
