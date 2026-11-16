/**
 * What the Feedback section shows, as plain functions (ADR-0051 decision 5):
 * the rating distribution rows, the signals beside the rating, and the words
 * for a workforce report. Everything a reader needs is text, never a colour or
 * an icon alone.
 */
import type {
  MarketplaceFeedbackItem,
  MarketplaceFeedbackScorecard,
  MarketplaceRatingBucket,
  MarketplaceSignals,
} from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

export interface DistributionRow {
  readonly stars: number;
  readonly count: number;
  /** 0 to 100: the bar's width. */
  readonly share: number;
}

/** Five rows, five stars first, each with its count and its share of the reviews. */
export function distributionRows(
  bucket: Pick<MarketplaceRatingBucket, 'distribution' | 'rating_count'>,
): DistributionRow[] {
  return [5, 4, 3, 2, 1].map((stars) => {
    const count = bucket.distribution[stars - 1] ?? 0;
    return { stars, count, share: bucket.rating_count > 0 ? Math.round((count / bucket.rating_count) * 100) : 0 };
  });
}

/** "Rated 4.3 out of 5 by 12 reviews", or "No reviews yet". */
export function reviewRatingText(bucket: Pick<MarketplaceRatingBucket, 'rating_avg' | 'rating_count'>): string {
  if (bucket.rating_avg === null || bucket.rating_count === 0)
    return t('marketplace.feedback.noReviews', 'No reviews yet');
  return t('marketplace.feedback.rated', 'Rated {{rating}} out of 5 by {{count}} reviews', {
    rating: bucket.rating_avg.toFixed(1),
    count: bucket.rating_count,
  });
}

/** A rate as a whole percentage, or a dash for a rate that was never measured. */
export function percentText(rate: number | null | undefined): string {
  return rate === null || rate === undefined ? '—' : `${Math.round(rate * 100)}%`;
}

export interface SignalFigure {
  readonly key: string;
  readonly label: string;
  readonly value: string;
}

/** The four signals of a listing or of one version, labelled. A dash means nothing was measured yet. */
export function signalFigures(signals: MarketplaceSignals | undefined): SignalFigure[] {
  return [
    {
      key: 'active',
      label: t('marketplace.signals.activeHires', 'Active hires'),
      value: String(signals?.active_hires ?? 0),
    },
    {
      key: 'retention',
      label: t('marketplace.signals.retention', '30-day retention'),
      value: percentText(signals?.retention_30d),
    },
    {
      key: 'dismissal',
      label: t('marketplace.signals.dismissal', 'Dismissal rate'),
      value: percentText(signals?.dismissal_rate),
    },
    {
      key: 'upgrade',
      label: t('marketplace.signals.upgrade', 'Upgrade adoption'),
      value: percentText(signals?.upgrade_adoption),
    },
  ];
}

/** The outcome of a workforce report as a word. */
export function outcomeLabel(outcome: string | undefined): string {
  switch (outcome) {
    case 'kept':
      return t('marketplace.feedback.outcomeKept', 'Kept');
    case 'demoted':
      return t('marketplace.feedback.outcomeDemoted', 'Demoted');
    case 'dismissed':
      return t('marketplace.feedback.outcomeDismissed', 'Dismissed');
    case undefined:
      return '';
    default:
      return outcome ?? '';
  }
}

/** The scorecard summary of a report as short lines of text. Only the four platform numbers exist. */
export function scorecardLines(scorecard: MarketplaceFeedbackScorecard | undefined): string[] {
  if (scorecard === undefined) return [];
  const lines: string[] = [];
  if (scorecard.failure_rate !== undefined) {
    lines.push(
      t('marketplace.feedback.failureRate', 'Failure rate {{value}}', { value: percentText(scorecard.failure_rate) }),
    );
  }
  if (scorecard.breaker_trips !== undefined) {
    lines.push(t('marketplace.feedback.breakerTrips', 'Breaker trips {{value}}', { value: scorecard.breaker_trips }));
  }
  if (scorecard.cost_vs_median !== undefined) {
    lines.push(
      t('marketplace.feedback.costVsMedian', 'Cost {{value}} times the median', { value: scorecard.cost_vs_median }),
    );
  }
  if (scorecard.active_days !== undefined) {
    lines.push(t('marketplace.feedback.activeDays', 'Active {{value}} days', { value: scorecard.active_days }));
  }
  return lines;
}

/** "Rated 4 out of 5" for one review. */
export function itemRatingText(item: Pick<MarketplaceFeedbackItem, 'rating'>): string {
  return item.rating === null
    ? ''
    : t('marketplace.feedback.itemRated', 'Rated {{rating}} out of 5', { rating: item.rating });
}
