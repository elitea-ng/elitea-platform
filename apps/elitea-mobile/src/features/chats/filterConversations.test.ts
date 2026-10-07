import { filterConversations } from './filterConversations';

describe('chat search (on the device, over loaded or cached titles)', () => {
  const rows = [{ name: 'Q3 planning' }, { name: 'Film club charter' }, { name: null }];
  it('matches a part of the title, ignoring case and spaces around the query', () => {
    expect(filterConversations(rows, ' q3 ')).toEqual([{ name: 'Q3 planning' }]);
  });
  it('an untitled chat is found as "Untitled conversation"', () => {
    expect(filterConversations(rows, 'untitled')).toEqual([{ name: null }]);
  });
  it('a blank query keeps every row', () => {
    expect(filterConversations(rows, '  ')).toHaveLength(3);
  });
});
