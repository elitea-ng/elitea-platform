import { describe, expect, it } from 'vitest';

import { validateAgentDraft } from './agentDraftValidation.helpers';

describe('validateAgentDraft', () => {
  it('returns no errors for a fully valid draft', () => {
    expect(
      validateAgentDraft({
        name: 'My Agent',
        description: 'Does things.',
        welcome_message: 'Hi!',
        conversation_starters: ['Hello', 'What can you do?'],
      }),
    ).toStrictEqual({});
  });

  it('requires a non-blank name', () => {
    expect(validateAgentDraft({ name: '', description: 'd' }).name).toBe('Name is required');
    expect(validateAgentDraft({ name: '   ', description: 'd' }).name).toBe('Name is required');
    expect(validateAgentDraft({ description: 'd' }).name).toBe('Name is required');
  });

  it('rejects a name over MAX_NAME_LENGTH (32)', () => {
    const errors = validateAgentDraft({ name: 'x'.repeat(33), description: 'd' });
    expect(errors.name).toBe('Name must be 32 characters or less');
  });

  it('accepts a name at exactly the 32-character limit', () => {
    const errors = validateAgentDraft({ name: 'x'.repeat(32), description: 'd' });
    expect(errors.name).toBeUndefined();
  });

  it('requires a non-blank description', () => {
    expect(validateAgentDraft({ name: 'n', description: '' }).description).toBe('Description is required');
    expect(validateAgentDraft({ name: 'n' }).description).toBe('Description is required');
  });

  it('rejects a description over MAX_DESCRIPTION_LENGTH (2000)', () => {
    const errors = validateAgentDraft({ name: 'n', description: 'x'.repeat(2305) });
    expect(errors.description).toBe('Description must be 2000 characters or less');
  });

  it('rejects a welcome message over MAX_WELCOME_MESSAGE_LENGTH (768)', () => {
    const errors = validateAgentDraft({ name: 'n', description: 'd', welcome_message: 'x'.repeat(769) });
    expect(errors.welcome_message).toBe('Welcome message must be 768 characters or less');
  });

  it('does not flag a missing welcome message (optional field)', () => {
    expect(validateAgentDraft({ name: 'n', description: 'd' }).welcome_message).toBeUndefined();
  });

  it('rejects more than MAX_CONVERSATION_STARTERS (4) starters', () => {
    const errors = validateAgentDraft({
      name: 'n',
      description: 'd',
      conversation_starters: ['a', 'b', 'c', 'd', 'e'],
    });
    expect(errors.conversation_starters).toBe('Maximum 4 conversation starters allowed');
  });

  it('rejects a starter over MAX_CONVERSATION_STARTER_LENGTH (768)', () => {
    const errors = validateAgentDraft({
      name: 'n',
      description: 'd',
      conversation_starters: ['x'.repeat(769)],
    });
    expect(errors.conversation_starters_length).toBe('Each starter must be 768 characters or less');
  });

  it('ignores undefined entries in conversation_starters when checking length', () => {
    const errors = validateAgentDraft({
      name: 'n',
      description: 'd',
      conversation_starters: [undefined, 'short'],
    });
    expect(errors.conversation_starters_length).toBeUndefined();
  });

  it('can report multiple field errors at once', () => {
    const errors = validateAgentDraft({ name: '', description: '' });
    expect(errors).toStrictEqual({ name: 'Name is required', description: 'Description is required' });
  });
});
