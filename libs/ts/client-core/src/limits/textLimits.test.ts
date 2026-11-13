import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { DESCRIPTION_MAX_LENGTH, NAME_MAX_LENGTH, characterCount, clipToLimit, exceedsLimit, textLimitState } from './index';

describe('the entity text limits', () => {
  it('are the server numbers', () => {
    assert.equal(NAME_MAX_LENGTH, 128);
    assert.equal(DESCRIPTION_MAX_LENGTH, 2000);
  });
});

describe('characterCount', () => {
  it('counts code points like the server counts runes', () => {
    assert.equal(characterCount('abc'), 3);
    assert.equal(characterCount('я'.repeat(128)), 128);
    assert.equal(characterCount('😀'.repeat(128)), 128);
    assert.equal('😀'.repeat(128).length, 256);
    assert.equal(characterCount(''), 0);
  });
});

describe('textLimitState', () => {
  it('is exceeded one character past the limit and not at it', () => {
    assert.equal(textLimitState('a'.repeat(128), NAME_MAX_LENGTH).exceeded, false);
    const over = textLimitState('a'.repeat(129), NAME_MAX_LENGTH);
    assert.deepEqual(over, { count: 129, remaining: 0, over: 1, exceeded: true });
    assert.equal(textLimitState('字'.repeat(2000), DESCRIPTION_MAX_LENGTH).exceeded, false);
    assert.equal(textLimitState('字'.repeat(2001), DESCRIPTION_MAX_LENGTH).exceeded, true);
  });
  it('reports the characters left', () => {
    assert.equal(textLimitState('abc', 10).remaining, 7);
  });
});

describe('exceedsLimit', () => {
  it('treats a missing value as within the limit', () => {
    assert.equal(exceedsLimit(undefined, 128), false);
    assert.equal(exceedsLimit(null, 128), false);
    assert.equal(exceedsLimit('a'.repeat(129), 128), true);
  });
});

describe('clipToLimit', () => {
  it('cuts derived text at a code point boundary', () => {
    const clipped = clipToLimit('😀'.repeat(200), 128);
    assert.equal(characterCount(clipped), 128);
    assert.equal(clipToLimit('short', 128), 'short');
  });
});
