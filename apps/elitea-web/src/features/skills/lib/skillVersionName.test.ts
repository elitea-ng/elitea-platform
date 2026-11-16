import { describe, expect, it } from 'vitest';

import { isDuplicateVersionName, nextVersionName } from './skillVersionName';

describe('nextVersionName', () => {
  it('starts at v1 for a skill that only has base', () => {
    expect(nextVersionName(['base'])).toBe('v1');
  });
  it('goes one past the highest numbered version', () => {
    expect(nextVersionName(['base', 'v1', 'v4', 'draft'])).toBe('v5');
  });
  it('keeps a bare numeric style', () => {
    expect(nextVersionName(['base', '2'])).toBe('3');
  });
});

describe('isDuplicateVersionName', () => {
  it('ignores case and surrounding whitespace', () => {
    expect(isDuplicateVersionName(' V1 ', ['base', 'v1'])).toBe(true);
    expect(isDuplicateVersionName('v2', ['base', 'v1'])).toBe(false);
    expect(isDuplicateVersionName('', ['base'])).toBe(false);
  });
});
