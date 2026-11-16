import { listingFeedbackOf, listingIdOf, ratingSentence, signalsSentence } from '@/api/queries/marketplaceFeedback';

const feedback = {
  items: [
    { id: '1', kind: 'review', version_name: 'v2', rating: 4, body: 'Works.', author: { name: 'Dana' }, state: 'visible' },
    {
      id: '2',
      kind: 'workforce_report',
      version_name: 'v1',
      rating: null,
      body: 'Kept failing.',
      outcome: 'dismissed',
      author: { name: 'Workforce report from a hiring project' },
      state: 'visible',
    },
    { id: '3', kind: 'review', version_name: 'v2', rating: 1, body: 'x', author: { name: 'Eve' }, state: 'hidden' },
    { id: '4', kind: 'mystery', version_name: 'v2', rating: null, body: '', author: { name: '' }, state: 'visible' },
  ],
  total: 4,
  overall: { rating_avg: 3.5, rating_count: 2 },
  signals: { listing: { active_hires: 12, retention_30d: 0.8, dismissal_rate: 0.2, upgrade_adoption: null } },
};

describe('marketplace feedback', () => {
  it('finds the listing of a public agent', () => {
    expect(listingIdOf({ rows: [{ id: '7' }] })).toBe('7');
    expect(listingIdOf({ rows: [] })).toBeNull();
    expect(listingIdOf('nope')).toBeNull();
  });

  it('keeps visible reviews and reports, drops hidden and unknown items', () => {
    const view = listingFeedbackOf(feedback);
    expect(view?.items.map((item) => item.id)).toEqual(['1', '2']);
    expect(view?.items[1]).toMatchObject({ isReport: true, outcome: 'dismissed', rating: null });
  });

  it('writes the rating and the signals as text, leaving out what was never measured', () => {
    const view = listingFeedbackOf(feedback);
    expect(view).not.toBeNull();
    expect(ratingSentence(view!)).toBe('Rated 3.5 out of 5 by 2 reviews');
    expect(signalsSentence(view!)).toBe('Active hires 12 · 30-day retention 80% · Dismissal rate 20%');
    expect(ratingSentence({ ratingAvg: null, ratingCount: 0 })).toBe('No reviews yet');
  });

  it('answers null for a body that is not feedback', () => {
    expect(listingFeedbackOf(null)).toBeNull();
  });
});
