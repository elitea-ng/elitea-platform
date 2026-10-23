/**
 * The platform model form's description (legacy issue 6766): read from the
 * stored row, written trimmed, and written even when blank so a cleared
 * description does not survive the merge with the stored object. And the
 * per-model DIAL protocol (legacy issue 6707): offered for a DIAL credential
 * only, and removed rather than sent stale or empty.
 */
import { describe, expect, it } from 'vitest';

import type { PlatformModel } from './api/adminLlmPlatformModelsApi';
import { formOf, modelDraftOf, showsDialProtocol } from './platformModelForm';

const STORED = {
  elitea_title: 'gpt5',
  type: 'llm_model',
  model_name: 'gpt-5',
  credential_name: 'openai',
  low_tier: false,
  high_tier: true,
  data: { name: 'gpt-5', description: 'Fast for everyday tasks', ai_credentials: { elitea_title: 'openai' } },
} as unknown as PlatformModel;

describe('platform model description', () => {
  it('opens a new model with no description and an edited one with its stored text', () => {
    expect(formOf(undefined, ['llm_model']).description).toBe('');
    expect(formOf(STORED, ['llm_model']).description).toBe('Fast for everyday tasks');
  });

  it('writes the trimmed description over the stored one', () => {
    const form = { ...formOf(STORED, ['llm_model']), description: '  Best for coding and agents ' };
    expect(modelDraftOf(form, STORED.data).data['description']).toBe('Best for coding and agents');
  });

  it('writes a cleared description as blank, so the stored text does not survive the merge', () => {
    const form = { ...formOf(STORED, ['llm_model']), description: '   ' };
    expect(modelDraftOf(form, STORED.data).data['description']).toBe('');
  });

  it('writes no description for a model kind that has none', () => {
    const form = { ...formOf(undefined, ['embedding_model']), type: 'embedding_model', description: 'ignored' };
    expect('description' in modelDraftOf(form, undefined).data).toBe(false);
  });
});

describe('platform model DIAL protocol', () => {
  const DIAL_STORED = {
    ...STORED,
    credential_name: 'dial',
    data: { name: 'gpt-5', ai_credentials: { elitea_title: 'dial' }, dial_protocol: 'openai' },
  } as unknown as PlatformModel;

  it('reads the stored protocol, and reads azure or nothing as the default', () => {
    expect(formOf(DIAL_STORED, ['llm_model']).dialProtocol).toBe('openai');
    expect(formOf(STORED, ['llm_model']).dialProtocol).toBe('');
    const azure = { ...STORED, data: { ...STORED.data, dial_protocol: 'azure' } } as unknown as PlatformModel;
    expect(formOf(azure, ['llm_model']).dialProtocol).toBe('');
  });

  it('shows the select for a chat model on a DIAL credential only', () => {
    const form = formOf(DIAL_STORED, ['llm_model']);
    expect(showsDialProtocol(form, 'ai_dial')).toBe(true);
    expect(showsDialProtocol(form, 'open_ai')).toBe(false);
    expect(showsDialProtocol(form, undefined)).toBe(false);
    expect(showsDialProtocol({ ...form, type: 'embedding_model' }, 'ai_dial')).toBe(false);
  });

  it('writes the chosen protocol for a DIAL credential', () => {
    const form = { ...formOf(DIAL_STORED, ['llm_model']), dialProtocol: 'anthropic' as const };
    expect(modelDraftOf(form, DIAL_STORED.data, 'ai_dial').data['dial_protocol']).toBe('anthropic');
  });

  it('removes the key for the default, so the server writes nothing', () => {
    const form = { ...formOf(DIAL_STORED, ['llm_model']), dialProtocol: '' as const };
    expect('dial_protocol' in modelDraftOf(form, DIAL_STORED.data, 'ai_dial').data).toBe(false);
  });

  it('removes a stale protocol when the credential is no longer a DIAL one', () => {
    const form = { ...formOf(DIAL_STORED, ['llm_model']), credential: 'openai' };
    expect('dial_protocol' in modelDraftOf(form, DIAL_STORED.data, 'open_ai').data).toBe(false);
  });

  it('leaves the stored protocol alone when the server did not report credential types', () => {
    const form = formOf(DIAL_STORED, ['llm_model']);
    expect(modelDraftOf(form, DIAL_STORED.data).data['dial_protocol']).toBe('openai');
  });
});
