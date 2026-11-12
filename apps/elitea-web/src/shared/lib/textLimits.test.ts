import { describe, expect, it } from 'vitest';

import { DESCRIPTION_MAX_LENGTH, NAME_MAX_LENGTH, clipToLimit, exceedsLimit, isNearLimit } from './textLimits';

describe('entity text limits (issue 1968)', () => {
  it('are the contract numbers', () => {
    expect(NAME_MAX_LENGTH).toBe(128);
    expect(DESCRIPTION_MAX_LENGTH).toBe(2000);
  });

  it('refuse one character past the limit and not at it', () => {
    expect(exceedsLimit('a'.repeat(128), NAME_MAX_LENGTH)).toBe(false);
    expect(exceedsLimit('a'.repeat(129), NAME_MAX_LENGTH)).toBe(true);
    expect(exceedsLimit('字'.repeat(2000), DESCRIPTION_MAX_LENGTH)).toBe(false);
    expect(exceedsLimit('字'.repeat(2001), DESCRIPTION_MAX_LENGTH)).toBe(true);
  });

  it('shows a counter only in the last fifth of the limit', () => {
    expect(isNearLimit('a'.repeat(50), NAME_MAX_LENGTH)).toBe(false);
    expect(isNearLimit('a'.repeat(103), NAME_MAX_LENGTH)).toBe(true);
  });

  it('cuts a derived conversation name at a character boundary', () => {
    expect(Array.from(clipToLimit(`Chat with ${'😀'.repeat(200)}`, NAME_MAX_LENGTH))).toHaveLength(128);
  });
});
