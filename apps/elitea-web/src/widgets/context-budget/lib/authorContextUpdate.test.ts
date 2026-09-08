import { describe, expect, it } from 'vitest';

import {
  buildContextBudgetUpdate,
  readContextBlock,
  selectMaxContextTokens,
  validateMaxContextTokens,
} from './authorContextUpdate';

describe('readContextBlock / selectMaxContextTokens', () => {
  it('reads the top-level column', () => {
    expect(selectMaxContextTokens({ default_context_management: { max_context_tokens: 32_000 } })).toBe(32_000);
  });

  it('falls back to a copy an older client nested inside personalization', () => {
    // Without this fallback a profile last saved by that client reads as
    // "never configured", and the next save would overwrite its real value.
    expect(
      selectMaxContextTokens({ personalization: { default_context_management: { max_context_tokens: 10_000 } } }),
    ).toBe(10_000);
  });

  it('prefers the column over the nested copy', () => {
    expect(
      selectMaxContextTokens({
        default_context_management: { max_context_tokens: 32_000 },
        personalization: { default_context_management: { max_context_tokens: 10_000 } },
      }),
    ).toBe(32_000);
  });

  it('reports no budget for an account that has never saved one', () => {
    expect(selectMaxContextTokens(undefined)).toBeUndefined();
    expect(selectMaxContextTokens({})).toBeUndefined();
    expect(readContextBlock(undefined)).toEqual({});
  });
});

describe('validateMaxContextTokens', () => {
  it('accepts a whole number inside the range the server enforces', () => {
    expect(validateMaxContextTokens('32000')).toBe(32_000);
    expect(validateMaxContextTokens(' 1000 ')).toBe(1000);
  });

  it('refuses what the server would refuse', () => {
    expect(validateMaxContextTokens('999')).toBeUndefined();
    expect(validateMaxContextTokens('10000001')).toBeUndefined();
    expect(validateMaxContextTokens('1000.5')).toBeUndefined();
    expect(validateMaxContextTokens('abc')).toBeUndefined();
    expect(validateMaxContextTokens('')).toBeUndefined();
  });
});

describe('buildContextBudgetUpdate', () => {
  const author = {
    name: 'Ada',
    description: 'Engineer',
    avatar: 'https://example.test/a.png',
    personalization: { persona: 'default', default_summarization: { enable_summarization: true } },
    default_context_management: { enabled: true, preserve_recent_messages: 7, max_context_tokens: 10_000 },
  };

  it('carries name, description and avatar forward', () => {
    // `UpdateAuthor` UPSERTS these from the body; omitting one blanks the
    // stored value for every other surface that reads the profile.
    const body = buildContextBudgetUpdate(author, 32_000);
    expect(body.name).toBe('Ada');
    expect(body.description).toBe('Engineer');
    expect(body.avatar).toBe('https://example.test/a.png');
  });

  it('carries personalization forward byte for byte, including a legacy nested block', () => {
    // The PUT REPLACES personalization. Stripping the nested summarization
    // block here — with no top-level replacement sent for it — would delete
    // that profile's summarization settings.
    const legacy = { personalization: { default_summarization: { enable_summarization: true } } };
    expect(buildContextBudgetUpdate(legacy, 32_000).personalization).toEqual(legacy.personalization);
  });

  it('changes the budget and nothing else in the context block', () => {
    const body = buildContextBudgetUpdate(author, 32_000);
    expect(body.default_context_management).toEqual({
      enabled: true,
      preserve_recent_messages: 7,
      max_context_tokens: 32_000,
    });
  });

  it('turns the context manager on, so a budget the reader typed is not inert', () => {
    const off = { default_context_management: { enabled: false, max_context_tokens: 5000 } };
    expect(buildContextBudgetUpdate(off, 32_000).default_context_management).toMatchObject({
      enabled: true,
      max_context_tokens: 32_000,
    });
  });

  it('sends no summarization block at all, which is what keeps the stored one', () => {
    // `default_summarization` is COALESCEd server-side: a body that does not
    // mention it keeps whatever is stored. Sending an empty one would not.
    expect(buildContextBudgetUpdate(author, 32_000)).not.toHaveProperty('default_summarization');
  });

  it('works for an account with no stored profile at all', () => {
    const body = buildContextBudgetUpdate(undefined, 64_000);
    expect(body).toEqual({
      personalization: {},
      default_context_management: { enabled: true, max_context_tokens: 64_000 },
    });
  });
});
