import { describe, expect, it } from 'vitest';

import { applicationCreationSchema } from './validation';

describe('applicationCreationSchema', () => {
  it('accepts a minimal valid draft', () => {
    const result = applicationCreationSchema.safeParse({
      name: 'Agent',
      description: 'Does things',
    });
    expect(result.success).toBe(true);
  });

  it('rejects a blank name', () => {
    const result = applicationCreationSchema.safeParse({
      name: '',
      description: 'Does things',
    });
    expect(result.success).toBe(false);
  });

  it('rejects a missing description', () => {
    const result = applicationCreationSchema.safeParse({ name: 'Agent' });
    expect(result.success).toBe(false);
  });

  it('accepts undefined and null conversation starters', () => {
    const result = applicationCreationSchema.safeParse({
      name: 'Agent',
      description: 'Does things',
      version_details: { conversation_starters: [undefined, null] },
    });
    expect(result.success).toBe(true);
  });

  it('accepts a non-blank conversation starter', () => {
    const result = applicationCreationSchema.safeParse({
      name: 'Agent',
      description: 'Does things',
      version_details: { conversation_starters: ['Hello there'] },
    });
    expect(result.success).toBe(true);
  });

  it('preserves version tags for the submit callback', () => {
    const result = applicationCreationSchema.safeParse({
      name: 'Pipeline',
      description: 'Does things',
      version_details: { conversation_starters: [], tags: ['mcp'] },
    });

    expect(result.success).toBe(true);
    if (result.success) expect(result.data.version_details?.tags).toEqual(['mcp']);
  });

  it('rejects a whitespace-only conversation starter', () => {
    const result = applicationCreationSchema.safeParse({
      name: 'Agent',
      description: 'Does things',
      version_details: { conversation_starters: ['   '] },
    });
    expect(result.success).toBe(false);
  });

  it('rejects an empty-string conversation starter', () => {
    const result = applicationCreationSchema.safeParse({
      name: 'Agent',
      description: 'Does things',
      version_details: { conversation_starters: [''] },
    });
    expect(result.success).toBe(false);
  });

  // Issue 1968: the inputs do not cut, so the schema blocks Save past the limits.
  it('accepts a name and description at the limits and rejects one character more', () => {
    expect(applicationCreationSchema.safeParse({ name: 'n'.repeat(32), description: 'd'.repeat(2000) }).success).toBe(true);
    const longName = applicationCreationSchema.safeParse({ name: 'n'.repeat(33), description: 'ok' });
    expect(longName.success).toBe(false);
    expect(longName.error?.issues[0]?.message).toBe('Name must be 32 characters or fewer');
    const longDescription = applicationCreationSchema.safeParse({ name: 'Agent', description: 'd'.repeat(2001) });
    expect(longDescription.error?.issues[0]?.message).toBe('Description must be 2000 characters or fewer');
  });

  it('counts characters, not UTF-16 units', () => {
    expect(applicationCreationSchema.safeParse({ name: '😀'.repeat(32), description: '字'.repeat(2000) }).success).toBe(true);
  });
});
