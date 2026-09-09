import { describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import { formatMemoryTags, memoryServerErrorMessage, parseMemoryTags } from './longTermMemoryHelpers';

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

const url = 'http://localhost/api/v2/elitea_core/memories/prompt_lib/1';

describe('memoryServerErrorMessage', () => {
  it("reads the server's own {\"error\": ...} body", () => {
    const error = new EliteaApiError({ kind: 'http', status: 400, url, body: { error: 'content is required' } });
    expect(memoryServerErrorMessage(error, 'fallback')).toBe('content is required');
  });

  it('falls back to {"message": ...} when "error" is absent', () => {
    const error = new EliteaApiError({ kind: 'http', status: 400, url, body: { message: 'too long' } });
    expect(memoryServerErrorMessage(error, 'fallback')).toBe('too long');
  });

  it('reads a plain-string body verbatim', () => {
    const error = new EliteaApiError({ kind: 'http', status: 400, url, body: 'memory not found' });
    expect(memoryServerErrorMessage(error, 'fallback')).toBe('memory not found');
  });

  it('uses the fallback for a non-EliteaApiError', () => {
    expect(memoryServerErrorMessage(new Error('network down'), 'fallback')).toBe('fallback');
  });

  it('uses the fallback when the body carries no usable message', () => {
    const error = new EliteaApiError({ kind: 'http', status: 500, url, body: {} });
    expect(memoryServerErrorMessage(error, 'fallback')).toBe('fallback');
  });
});
