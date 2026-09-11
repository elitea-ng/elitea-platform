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
});
