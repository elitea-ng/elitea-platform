import { describe, expect, it } from 'vitest';

import {
  buildContextBudgetUpdate,
  readContextBlock,
  selectBudgetMode,
} from './authorContextUpdate';

describe('readContextBlock / selectBudgetMode', () => {
  it('reads the top-level column', () => {
    expect(selectBudgetMode({ default_context_management: { budget_mode: 'full' } })).toBe('full');
  });

  it('falls back to a copy an older client nested inside personalization', () => {
    // Without this fallback a profile last saved by that client reads as
    // "never configured", and the next save would overwrite its real value.
    expect(
      selectBudgetMode({ personalization: { default_context_management: { budget_mode: 'balanced' } } }),
    ).toBe('balanced');
  });

  it('prefers the column over the nested copy', () => {
    expect(
      selectBudgetMode({
        default_context_management: { budget_mode: 'full' },
        personalization: { default_context_management: { budget_mode: 'balanced' } },
      }),
    ).toBe('full');
  });

  it('reports no budget for an account that has never saved one', () => {
    expect(selectBudgetMode(undefined)).toBe('balanced');
    expect(selectBudgetMode({})).toBe('balanced');
    expect(readContextBlock(undefined)).toEqual({});
  });
});

describe('buildContextBudgetUpdate', () => {
  const author = {
    name: 'Ada',
    description: 'Engineer',
    avatar: 'https://example.test/a.png',
    personalization: { persona: 'default', default_summarization: { enable_summarization: true } },
    default_context_management: { enabled: true, preserve_recent_messages: 7, budget_mode: 'balanced' },
  };

  it('carries name, description and avatar forward', () => {
    // `UpdateAuthor` UPSERTS these from the body; omitting one blanks the
    // stored value for every other surface that reads the profile.
    const body = buildContextBudgetUpdate(author, 'full');
    expect(body.name).toBe('Ada');
    expect(body.description).toBe('Engineer');
    expect(body.avatar).toBe('https://example.test/a.png');
  });

  it('carries personalization forward byte for byte, including a legacy nested block', () => {
    // The PUT REPLACES personalization. Stripping the nested summarization
    // block here — with no top-level replacement sent for it — would delete
    // that profile's summarization settings.
    const legacy = { personalization: { default_summarization: { enable_summarization: true } } };
    expect(buildContextBudgetUpdate(legacy, 'full').personalization).toEqual(legacy.personalization);
  });

  it('changes the budget and nothing else in the context block', () => {
    const body = buildContextBudgetUpdate(author, 'full');
    expect(body.default_context_management).toEqual({
      enabled: true,
      preserve_recent_messages: 7,
      budget_mode: 'full',
    });
  });

  it('preserves disabled compaction while changing the preset', () => {
    const off = { default_context_management: { enabled: false, max_context_tokens: 5000 } };
    expect(buildContextBudgetUpdate(off, 'full').default_context_management).toMatchObject({
      enabled: false,
      budget_mode: 'full',
    });
  });

  it('sends no summarization block at all, which is what keeps the stored one', () => {
    // `default_summarization` is COALESCEd server-side: a body that does not
    // mention it keeps whatever is stored. Sending an empty one would not.
    expect(buildContextBudgetUpdate(author, 'full')).not.toHaveProperty('default_summarization');
  });

  it('works for an account with no stored profile at all', () => {
    const body = buildContextBudgetUpdate(undefined, 'balanced');
    expect(body).toEqual({
      personalization: {},
      default_context_management: { budget_mode: 'balanced' },
    });
  });
});

it('does not reinterpret a legacy numeric preference as a combined window', () => {
  const author = { default_context_management: { max_context_tokens: 64000, enabled: true } };
  expect(selectBudgetMode(author)).toBe('balanced');
  expect(buildContextBudgetUpdate(author, 'full').default_context_management).toEqual({ enabled: true, budget_mode: 'full' });
  expect(author.default_context_management.max_context_tokens).toBe(64000);
});
