import { describe, expect, it } from 'vitest';

import { duplicateConversationName } from './useDuplicateConversation.hooks';

// Issue 1968: the server refuses names over 128 characters, so the suffix must fit inside it.
describe('duplicateConversationName', () => {
  it('appends the suffix to a short name unchanged', () => {
    expect(duplicateConversationName('Plan')).toBe('Plan (copy)');
  });

  it('clips a name at the limit so the duplicate is exactly 128 characters', () => {
    const name = duplicateConversationName('😀'.repeat(128));
    expect(Array.from(name)).toHaveLength(128);
    expect(name.endsWith(' (copy)')).toBe(true);
  });
});
