import { describe, expect, it } from 'vitest';

import { formatMemoryTags, parseMemoryTags } from './longTermMemoryHelpers';

describe('parseMemoryTags', () => {
  it('splits on commas and trims whitespace', () => {
    expect(parseMemoryTags(' preferences ,  engineering,tools ')).toEqual(['preferences', 'engineering', 'tools']);
  });

  it('drops empty entries from repeated or trailing commas', () => {
    expect(parseMemoryTags('a,,b,')).toEqual(['a', 'b']);
  });

  it('answers an empty array for blank input', () => {
    expect(parseMemoryTags('')).toEqual([]);
    expect(parseMemoryTags('   ')).toEqual([]);
  });
});

describe('formatMemoryTags', () => {
  it('joins with a comma and a space', () => {
    expect(formatMemoryTags(['a', 'b', 'c'])).toBe('a, b, c');
  });

  it('is the inverse of parseMemoryTags for a clean list', () => {
    const tags = ['preferences', 'engineering'];
    expect(parseMemoryTags(formatMemoryTags(tags))).toEqual(tags);
  });

  it('answers an empty string for an empty list', () => {
    expect(formatMemoryTags([])).toBe('');
  });
});
