import { describe, expect, it } from 'vitest';

import { filterEmptyStrings } from './agentDraft';

describe('filterEmptyStrings', () => {
  it('drops blank and whitespace-only entries', () => {
    expect(filterEmptyStrings(['hello', '', '  ', 'world'])).toEqual(['hello', 'world']);
  });

  it('returns an empty array when every entry is blank', () => {
    expect(filterEmptyStrings(['', '   '])).toEqual([]);
  });

  it('returns a new array, not the input reference', () => {
    const input = ['a'];
    expect(filterEmptyStrings(input)).not.toBe(input);
  });
});
