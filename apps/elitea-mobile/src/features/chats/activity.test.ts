import { activityGlyphOf, activityTitleBold, countNeedsYou, needsYouText } from '@/features/chats/activity';
import { buildChatRows, buildSearchRows } from '@/features/chats/chatListModel';

const NOW = Date.parse('2026-10-21T12:00:00Z');
const HOUR_AGO = '2026-10-21T11:00:00Z';
const TWO_DAYS_AGO = '2026-10-19T12:00:00Z';
const row = (id: number, extra: Record<string, unknown> = {}) => ({
  id,
  name: `c${id}`,
  created_at: HOUR_AGO,
  updated_at: HOUR_AGO,
  duration: -1,
  message_groups_count: 1,
  ...extra,
});

describe('the activity glyph of a Chats row (client contract 1.12)', () => {
  it('shows a running turn and a pause whatever the read state', () => {
    expect(activityGlyphOf({ activity: { state: 'running', since: HOUR_AGO }, unread: true, last_activity_at: HOUR_AGO }, NOW)).toBe('running');
    expect(activityGlyphOf({ activity: { state: 'needs_you', since: TWO_DAYS_AGO }, last_activity_at: TWO_DAYS_AGO }, NOW)).toBe('needs_you');
  });

  it('shows a filled check for a recent unread answer and a light check for a read one', () => {
    expect(activityGlyphOf({ activity: { state: 'idle', since: HOUR_AGO }, unread: true, last_activity_at: HOUR_AGO }, NOW)).toBe('unread');
    expect(activityGlyphOf({ activity: { state: 'idle', since: HOUR_AGO }, unread: false, last_activity_at: HOUR_AGO }, NOW)).toBe('done');
  });

  it('shows nothing for a finish older than a day, an unknown state as idle, and a row from a server before 1.12', () => {
    expect(activityGlyphOf({ activity: { state: 'idle' }, unread: true, last_activity_at: TWO_DAYS_AGO }, NOW)).toBeNull();
    expect(activityGlyphOf({ activity: { state: 'thinking' }, unread: false, last_activity_at: TWO_DAYS_AGO }, NOW)).toBeNull();
    expect(activityGlyphOf({ activity: { state: 'thinking' }, unread: false, last_activity_at: HOUR_AGO }, NOW)).toBe('done');
    expect(activityGlyphOf({}, NOW)).toBeNull();
  });

  it('bolds an unread title unless a turn runs', () => {
    expect(activityTitleBold({ unread: true })).toBe(true);
    expect(activityTitleBold({ unread: true, activity: { state: 'needs_you' } })).toBe(true);
    expect(activityTitleBold({ unread: true, activity: { state: 'running' } })).toBe(false);
    expect(activityTitleBold({ unread: false })).toBe(false);
    expect(activityTitleBold({})).toBe(false);
  });

  it('counts the rows waiting for the user', () => {
    expect(countNeedsYou([{ needsYou: true }, {}, { needsYou: true }, { needsYou: false }])).toBe(2);
    expect(needsYouText(2)).toBe('2 needs you');
  });
});

describe('the Chats list model carries the activity', () => {
  it('maps each row to its glyph, bold title and needs-you flag', () => {
    const rows = buildChatRows({
      loaded: [
        row(1, { activity: { state: 'running', since: HOUR_AGO }, last_activity_at: HOUR_AGO }),
        row(2, { activity: { state: 'needs_you', since: HOUR_AGO }, last_activity_at: HOUR_AGO, unread: true }),
        row(3, { activity: { state: 'idle', since: HOUR_AGO }, last_activity_at: HOUR_AGO, unread: true }),
        row(4),
      ],
      savedPinned: [],
      overlay: new Map(),
      now: NOW,
    });
    expect(rows.map((r) => [r.id, r.activity, r.unread, r.needsYou])).toEqual([
      [1, 'running', false, false],
      [2, 'needs_you', true, true],
      [3, 'unread', true, false],
      [4, null, false, false],
    ]);
  });

  it('keeps the activity on search results from the offline index and from loaded titles', () => {
    const indexed = row(5, { activity: { state: 'needs_you', since: HOUR_AGO }, last_activity_at: HOUR_AGO });
    const rows = buildSearchRows({
      hits: [{ id: 5, name: 'c5', updatedAt: HOUR_AGO, row: indexed, snippet: null }],
      loaded: [row(6, { name: 'c6 plan', unread: true, last_activity_at: HOUR_AGO, activity: { state: 'idle', since: HOUR_AGO } })],
      query: 'plan',
      overlay: new Map(),
      now: NOW,
    });
    expect(rows.map((r) => [r.id, r.activity, r.unread])).toEqual([
      [5, 'needs_you', false],
      [6, 'unread', true],
    ]);
  });
});
