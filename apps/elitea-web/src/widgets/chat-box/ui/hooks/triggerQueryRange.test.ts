import { describe, expect, it } from 'vitest';

import { triggerQueryEnd } from './triggerQueryRange';

describe('triggerQueryEnd (#6774)', () => {
  it('covers a non-ASCII query the key handler did not count', () => {
    // The handler saw only "#"; the text holds "#Пров".
    const content = 'ask #Пров';
    expect(content.slice(4, triggerQueryEnd(content, 4, '#', '#'))).toBe('#Пров');
  });

  it('stops at the first whitespace after the trigger', () => {
    const content = '#é tail';
    expect(triggerQueryEnd(content, 0, '#', '#')).toBe(2);
  });

  it('runs to the end of the text when nothing follows the query', () => {
    expect(triggerQueryEnd('@ana', 0, '@', '@')).toBe(4);
  });

  it('keeps the counted length when the trigger is no longer at the anchor', () => {
    expect(triggerQueryEnd('xyz', 1, '#', '#ab')).toBe(3);
    expect(triggerQueryEnd('', 0, '#', '#')).toBe(0);
    expect(triggerQueryEnd('#a', 5, '#', '#a')).toBe(2);
  });
});
