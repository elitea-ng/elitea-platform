import { describe, expect, it } from 'vitest';

import {
  distributionRows,
  outcomeLabel,
  percentText,
  reviewRatingText,
  scorecardLines,
  signalFigures,
} from './feedbackModel';

describe('feedbackModel', () => {
  it('lists five star rows, five first, with the share of the reviews', () => {
    const rows = distributionRows({ distribution: [1, 0, 0, 1, 2], rating_count: 4 });
    expect(rows.map((row) => row.stars)).toEqual([5, 4, 3, 2, 1]);
    expect(rows[0]).toEqual({ stars: 5, count: 2, share: 50 });
    expect(rows[2]).toEqual({ stars: 3, count: 0, share: 0 });
  });

  it('has no share to divide when there are no reviews', () => {
    expect(distributionRows({ distribution: [0, 0, 0, 0, 0], rating_count: 0 }).every((row) => row.share === 0)).toBe(
      true,
    );
  });

  it('writes the rating as text, or says there are no reviews', () => {
    expect(reviewRatingText({ rating_avg: 4.25, rating_count: 8 })).toBe('Rated 4.3 out of 5 by 8 reviews');
    expect(reviewRatingText({ rating_avg: null, rating_count: 0 })).toBe('No reviews yet');
  });

  it('shows a rate that was never measured as a dash, not as zero', () => {
    expect(percentText(null)).toBe('—');
    expect(percentText(undefined)).toBe('—');
    expect(percentText(0)).toBe('0%');
    expect(percentText(0.333)).toBe('33%');
  });

  it('labels the four signals, with dashes for what is missing', () => {
    expect(signalFigures(undefined).map((figure) => [figure.label, figure.value])).toEqual([
      ['Active hires', '0'],
      ['30-day retention', '—'],
      ['Dismissal rate', '—'],
      ['Upgrade adoption', '—'],
    ]);
  });

  it('turns an outcome and a scorecard into words', () => {
    expect(outcomeLabel('dismissed')).toBe('Dismissed');
    expect(outcomeLabel(undefined)).toBe('');
    expect(scorecardLines({ failure_rate: 0.25, breaker_trips: 2, cost_vs_median: 2.1, active_days: 14 })).toEqual([
      'Failure rate 25%',
      'Breaker trips 2',
      'Cost 2.1 times the median',
      'Active 14 days',
    ]);
    expect(scorecardLines(undefined)).toEqual([]);
  });
});
