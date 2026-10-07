import { chatSectionOf, groupChatsByDate } from '@/features/chats/sections';

// 2026-10-07 14:00 local time.
const NOW = new Date(2026, 9, 7, 14, 0, 0).getTime();
const at = (days: number, hour = 10) => new Date(2026, 9, 7 - days, hour, 0, 0).toISOString();

describe('Chats sections by local day', () => {
  it('puts each conversation in Today, Yesterday, Previous 7 days or Earlier', () => {
    expect(chatSectionOf(at(0, 0), NOW)).toBe('today');
    expect(chatSectionOf(at(1, 23), NOW)).toBe('yesterday');
    expect(chatSectionOf(at(6), NOW)).toBe('week');
    expect(chatSectionOf(at(7), NOW)).toBe('older');
    expect(chatSectionOf('not a date', NOW)).toBe('older');
  });

  it('keeps the server order inside a section and leaves empty sections out', () => {
    const rows = [
      { id: 1, updated_at: at(0, 12) },
      { id: 2, updated_at: at(0, 9) },
      { id: 3, updated_at: at(20) },
    ];
    expect(groupChatsByDate(rows, NOW).map((s) => [s.title, s.data.map((r) => r.id)])).toEqual([
      ['Today', [1, 2]],
      ['Earlier', [3]],
    ]);
  });
});
