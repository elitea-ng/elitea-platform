/**
 * The shared form model. Three properties are load-bearing and invisible in
 * the UI:
 *
 *  1. per-persona instructions survive the round trip, and every persona key
 *     exists on the way in even when the stored map is partial;
 *  2. the payload nests context-management/summarization INSIDE
 *     `personalization` — the Go handler reads no other top-level key, so a
 *     flat payload would save nothing (see the module header);
 *  3. the payload carries forward what the page does not edit. The upsert
 *     replaces `personalization` wholesale and re-writes
 *     `name`/`description`/`avatar` from the body, so a partial payload is
 *     silent data loss for the OTHER page's settings and for the avatar.
 */
import { describe, expect, it } from 'vitest';

import {
  buildAuthorUpdate,
  deserializeMemoryBlocks,
  deserializeSettingsProfile,
  serializeSettingsProfile,
} from './settingsProfileForm';

describe('serializeSettingsProfile', () => {
  it('merges a partial stored instructions map over an empty slot for every persona', () => {
    const values = serializeSettingsProfile({
      personalization: { persona: 'qa', personality_instructions: { qa: 'be terse' } },
    });

    expect(values.persona).toBe('qa');
    expect(values.personality_instructions).toEqual({
      generic: '',
      qa: 'be terse',
      nerdy: '',
      quirky: '',
      cynical: '',
      none: '',
      bare: '',
    });
  });

  it('defaults to the `generic` persona and empty slots when the profile has no personalization', () => {
    const values = serializeSettingsProfile(undefined);

    expect(values.persona).toBe('generic');
    expect(Object.values(values.personality_instructions).every((slot) => slot === '')).toBe(true);
  });

  it('reads the context-management and summarization blocks from inside personalization', () => {
    const values = serializeSettingsProfile({
      personalization: {
        default_context_management: {
          enabled: false,
          max_context_tokens: 12_345,
          preserve_recent_messages: 9,
          enable_context_editing: true,
        },
        default_summarization: {
          enable_summarization: false,
          summary_instructions: 'short please',
          summary_model_name: 'gpt-4o',
          summary_model_project_id: '7',
          target_summary_tokens: 512,
        },
      },
    });

    expect(values.context_enabled).toBe(false);
    expect(values.budget_mode).toBe('balanced');
    expect(values.preserve_recent_messages).toBe(9);
    expect(values.enable_context_editing).toBe(true);
    expect(values.enable_summarization).toBe(false);
    expect(values.summary_llm_settings).toEqual({
      instructions: 'short please',
      model_name: 'gpt-4o',
      model_project_id: '7',
      max_tokens: 512,
    });
  });

  it('falls back to the selected project as the summary model owner when none is stored', () => {
    const values = serializeSettingsProfile({ personalization: {} }, 'proj-9');

    expect(values.summary_llm_settings.model_project_id).toBe('proj-9');
  });
});

describe('deserializeSettingsProfile', () => {
  it('round-trips the full per-persona map, not just the selected persona', () => {
    const instructions = { generic: 'g', qa: 'q', nerdy: '', quirky: '', cynical: '', none: '', bare: '' };
    const values = { ...serializeSettingsProfile(undefined), persona: 'qa', personality_instructions: instructions };

    const payload = deserializeSettingsProfile(values);

    expect(payload.persona).toBe('qa');
    expect(payload.personality_instructions).toEqual(instructions);
  });

  it('carries only the personality fields — the memory blocks travel at the top level', () => {
    const values = { ...serializeSettingsProfile(undefined), context_enabled: true, budget_mode: 'full' as const };

    const payload = deserializeSettingsProfile(values);

    expect('default_context_management' in payload).toBe(false);
    expect('default_summarization' in payload).toBe(false);
  });
});

describe('deserializeMemoryBlocks', () => {
  it('builds both blocks in the shape the two jsonb columns hold', () => {
    const values = {
      ...serializeSettingsProfile(undefined),
      context_enabled: true,
      budget_mode: 'full' as const,
      preserve_recent_messages: 3,
      enable_context_editing: true,
      enable_summarization: true,
      summary_llm_settings: { instructions: 'i', model_name: 'm', model_project_id: '2', max_tokens: 256 },
    };

    expect(deserializeMemoryBlocks(values)).toEqual({
      default_context_management: {
        enabled: true,
        budget_mode: 'full' as const,
        preserve_recent_messages: 3,
        enable_context_editing: true,
      },
      default_summarization: {
        enable_summarization: true,
        summary_instructions: 'i',
        summary_model_name: 'm',
        // #929 — the wire field is an int (`MemorySummarization.summary_model_
        // project_id` is `zod.int()`), even though `model_project_id` in form
        // state is the string project id like everywhere else in this app.
        summary_model_project_id: 2,
        target_summary_tokens: 256,
      },
    });
  });

  // #929 — the server rejected every autosave with 400 because this field
  // went out as the string project id. A non-numeric/absent value is
  // omitted (the server keeps the stored value), never sent malformed.
  it('sends summary_model_project_id as a number, and omits it when not a numeric string', () => {
    const values = {
      ...serializeSettingsProfile(undefined),
      summary_llm_settings: { instructions: '', model_name: '', model_project_id: 'not-a-number', max_tokens: '' as const },
    };

    const blocks = deserializeMemoryBlocks(values);

    expect('summary_model_project_id' in blocks.default_summarization).toBe(false);
  });

  // A numeric input the user has emptied is `''` — not a number, and not a
  // value. Sending it would be refused by the server's type check; omitting it
  // says what the empty box means: this default is not set.
  it('omits a cleared numeric field instead of sending an empty string', () => {
    const values = {
      ...serializeSettingsProfile(undefined),
      preserve_recent_messages: '' as const,
      summary_llm_settings: { instructions: '', model_name: '', model_project_id: null, max_tokens: '' as const },
    };

    const blocks = deserializeMemoryBlocks(values);

    expect('max_context_tokens' in blocks.default_context_management).toBe(false);
    expect('preserve_recent_messages' in blocks.default_context_management).toBe(false);
    expect('target_summary_tokens' in blocks.default_summarization).toBe(false);
    // The booleans are still answers and still travel.
    expect('enabled' in blocks.default_context_management).toBe(true);
  });
});

describe('buildAuthorUpdate', () => {
  it('sends both memory blocks at the top level', () => {
    const payload = buildAuthorUpdate(undefined, {
      ...serializeSettingsProfile(undefined),
      budget_mode: 'full',
    });

    expect(payload.default_context_management).toMatchObject({ budget_mode: 'full' });
    expect(payload.default_summarization).toBeDefined();
  });

  // A profile last written by the older client carries the blocks inside
  // `personalization`. They are read from there, moved to the top level, and
  // NOT written back into the blob — two homes for one setting is how the two
  // copies come to disagree.
  it('lifts a legacy nested block out of personalization and does not leave a copy behind', () => {
    const author = {
      personalization: {
        persona: 'generic',
        default_context_management: { max_context_tokens: 17_000 },
        default_summarization: { enable_summarization: false },
      },
    };
    const values = serializeSettingsProfile(author);

    expect(values.budget_mode).toBe('balanced');
    expect(values.enable_summarization).toBe(false);

    const payload = buildAuthorUpdate(author, values);

    expect(payload.default_context_management).toMatchObject({ budget_mode: 'balanced' });
    expect('default_context_management' in payload.personalization).toBe(false);
    expect('default_summarization' in payload.personalization).toBe(false);
  });

  // The top-level column wins over a stale nested copy of the same setting.
  it('prefers the top-level block over the personalization-nested one', () => {
    const values = serializeSettingsProfile({
      default_context_management: { budget_mode: 'full' },
      personalization: { default_context_management: { max_context_tokens: 17_000 } },
    });

    expect(values.budget_mode).toBe('full');
  });

  it('keeps personalization keys neither page edits, and the profile fields the upsert would blank', () => {
    const author = {
      name: 'Ada',
      description: 'about me',
      avatar: 'avatar.png',
      personalization: {
        persona: 'generic',
        // Owned by a different page; the PUT replaces the whole blob.
        default_internal_mcp_enabled: false,
        some_future_setting: { nested: true },
      },
    };
    const values = { ...serializeSettingsProfile(author), persona: 'nerdy' };

    const payload = buildAuthorUpdate(author, values);

    expect(payload.name).toBe('Ada');
    expect(payload.description).toBe('about me');
    expect(payload.avatar).toBe('avatar.png');
    expect(payload.personalization).toMatchObject({
      persona: 'nerdy',
      default_internal_mcp_enabled: false,
      some_future_setting: { nested: true },
    });
  });

  it('omits profile fields the fetched author does not carry, rather than sending empty strings', () => {
    const payload = buildAuthorUpdate(undefined, serializeSettingsProfile(undefined));

    expect('name' in payload).toBe(false);
    expect('avatar' in payload).toBe(false);
  });
});

it('keeps a numeric summary model owner and omits the owner when using the chat model', () => {
  const selected = serializeSettingsProfile({ default_summarization: { summary_model_name: 'summary', summary_model_project_id: 7 } }, '2');
  expect(selected.summary_llm_settings.model_project_id).toBe('7');
  expect(deserializeMemoryBlocks(selected).default_summarization.summary_model_project_id).toBe(7);
  const inherited = serializeSettingsProfile(undefined, '2');
  expect(deserializeMemoryBlocks(inherited).default_summarization).not.toHaveProperty('summary_model_project_id');
});
