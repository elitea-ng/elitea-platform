import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

import { useEditPipelineVersionFields } from './useEditPipelineVersionFields';

const VERSION = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  agent_type: 'pipeline',
  welcome_message: 'stored hello',
  variables: [{ name: 'x', value: '1' }],
  tags: [{ id: 4, name: 'sales', data: null }],
  llm_settings: { model_name: 'gpt-4o', model_project_id: 9 },
  meta: { step_limit: 40, internal_tools: ['attachments'] },
} as unknown as ApplicationVersionDetail;

describe('useEditPipelineVersionFields', () => {
  it('seeds every field off the version, and reports itself clean', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    expect(result.current.fields).toEqual({
      welcomeMessage: 'stored hello',
      variables: [{ name: 'x', value: '1' }],
      stepLimit: 40,
      internalTools: ['attachments'],
      // `max_tokens: -1` is `toAgentLlmSettings`' own normalisation of "no
      // ceiling named" — the read is strict so a half-built profile can never
      // reach the worker.
      llmSettings: { model_name: 'gpt-4o', model_project_id: 9, max_tokens: -1 },
      tags: [{ id: 4, name: 'sales', data: null }],
    });
    expect(result.current.isDirty).toBe(false);
  });

  it('defaults every field for a version that is not loaded yet', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(undefined));

    expect(result.current.fields).toEqual({
      welcomeMessage: '',
      variables: [],
      stepLimit: undefined,
      internalTools: [],
      llmSettings: undefined,
      tags: [],
    });
  });

  /** `tags.name` is NOT NULL, so a nameless entry cannot be stored and has no label to show. */
  it('drops a nameless tag and fills a missing tag id with 0', () => {
    const odd = { ...VERSION, tags: [{ name: null }, { name: 'ops' }] } as unknown as ApplicationVersionDetail;
    expect(renderHook(() => useEditPipelineVersionFields(odd)).result.current.fields.tags).toEqual([
      { id: 0, name: 'ops', data: null },
    ]);
  });

  it.each([
    ['version_details.welcome_message', 'typed', 'welcomeMessage', 'typed'],
    ['version_details.meta.step_limit', 12, 'stepLimit', 12],
    ['version_details.meta.internal_tools', ['attachments', 'planner'], 'internalTools', ['attachments', 'planner']],
    ['version_details.variables', [{ name: 'y', value: '2' }], 'variables', [{ name: 'y', value: '2' }]],
  ] as const)('routes %s into the field it owns', (path, value, field, expected) => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    act(() => {
      expect(result.current.applyFieldChange(path, value)).toBe(true);
    });

    expect(result.current.fields[field as 'welcomeMessage']).toEqual(expected);
    expect(result.current.isDirty).toBe(true);
  });

  it('refuses a path it does not own, so the caller can fall through to the RHF form', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));
    let handled = true;
    act(() => {
      handled = result.current.applyFieldChange('name', 'Renamed');
    });
    expect(handled).toBe(false);
  });

  it('takes a whole-object llm_settings replace, which is what the settings dialog Apply emits', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    act(() => {
      result.current.applyFieldChange('version_details.llm_settings', { model_name: 'qwen3.5', model_project_id: 9 });
    });

    expect(result.current.fields.llmSettings).toEqual({ model_name: 'qwen3.5', model_project_id: 9, max_tokens: -1 });
  });

  /**
   * `temperature` and `reasoning_effort` are mutually exclusive on the wire —
   * the worker refuses a profile carrying both — so a per-key write of one
   * must CLEAR the other, or setting a temperature on a version that stored
   * an effort loses the write to the XOR instead of replacing it.
   */
  it('clears reasoning_effort when a per-key temperature write lands, and the other way round', () => {
    const withEffort = {
      ...VERSION,
      llm_settings: { model_name: 'gpt-4o', model_project_id: 9, reasoning_effort: 'high' },
    } as unknown as ApplicationVersionDetail;
    const { result } = renderHook(() => useEditPipelineVersionFields(withEffort));

    act(() => {
      result.current.applyFieldChange('version_details.llm_settings.temperature', 0.6);
    });
    expect(result.current.fields.llmSettings).toEqual({ model_name: 'gpt-4o', model_project_id: 9, max_tokens: -1, temperature: 0.6 });

    act(() => {
      result.current.applyFieldChange('version_details.llm_settings.reasoning_effort', 'low');
    });
    expect(result.current.fields.llmSettings).toEqual({ model_name: 'gpt-4o', model_project_id: 9, max_tokens: -1, reasoning_effort: 'low' });
  });

  it('replaces the whole tag list through setTags', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    act(() => {
      result.current.setTags([{ id: -1, name: 'new', data: null }]);
    });

    expect(result.current.fields.tags).toEqual([{ id: -1, name: 'new', data: null }]);
    expect(result.current.isDirty).toBe(true);
  });

  /**
   * A tag the user just typed carries a placeholder id (`AgentTagEditor`), so
   * comparing by id would report the page dirty forever after a save that
   * stored that very tag.
   */
  it('compares tags by name, not by id', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    act(() => {
      result.current.setTags([{ id: -999, name: 'sales', data: null }]);
    });

    expect(result.current.isDirty).toBe(false);
  });

  /** The settings dialog hands back a fresh object each time, so identity comparison would report dirty from the first render. */
  it('compares llm_settings key by key, not by identity', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    act(() => {
      result.current.applyFieldChange('version_details.llm_settings', { model_name: 'gpt-4o', model_project_id: 9 });
    });

    expect(result.current.isDirty).toBe(false);
  });

  it('markSaved moves the baseline onto the current values, and reset moves the values back onto the baseline', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    act(() => {
      result.current.applyFieldChange('version_details.welcome_message', 'typed');
    });
    expect(result.current.isDirty).toBe(true);

    act(() => {
      result.current.markSaved();
    });
    expect(result.current.isDirty).toBe(false);
    expect(result.current.fields.welcomeMessage).toBe('typed');

    act(() => {
      result.current.applyFieldChange('version_details.welcome_message', 'typed again');
    });
    act(() => {
      result.current.reset();
    });
    expect(result.current.fields.welcomeMessage).toBe('typed');
    expect(result.current.isDirty).toBe(false);
  });

  /**
   * The detail query refetches on window focus and after every save, handing
   * back a fresh object for the SAME version. Re-seeding on object identity
   * would throw away whatever the user had typed since.
   */
  it('does NOT re-seed on a new response object for the same version id', () => {
    const { result, rerender } = renderHook(({ version }) => useEditPipelineVersionFields(version), {
      initialProps: { version: VERSION },
    });

    act(() => {
      result.current.applyFieldChange('version_details.welcome_message', 'typed');
    });
    rerender({ version: { ...VERSION } });

    expect(result.current.fields.welcomeMessage).toBe('typed');
  });

  it('DOES re-seed when the version identity changes', () => {
    const { result, rerender } = renderHook(({ version }) => useEditPipelineVersionFields(version), {
      initialProps: { version: VERSION },
    });

    act(() => {
      result.current.applyFieldChange('version_details.welcome_message', 'typed');
    });
    rerender({ version: { ...VERSION, id: '2', welcome_message: 'other version' } });

    expect(result.current.fields.welcomeMessage).toBe('other version');
    expect(result.current.isDirty).toBe(false);
  });

  it('coerces non-string and non-array writes rather than storing them', () => {
    const { result } = renderHook(() => useEditPipelineVersionFields(VERSION));

    act(() => {
      result.current.applyFieldChange('version_details.welcome_message', 42);
      result.current.applyFieldChange('version_details.meta.internal_tools', 'attachments');
      result.current.applyFieldChange('version_details.meta.step_limit', 'twelve');
      result.current.applyFieldChange('version_details.variables', 'nope');
    });

    expect(result.current.fields.welcomeMessage).toBe('');
    expect(result.current.fields.internalTools).toEqual([]);
    expect(result.current.fields.stepLimit).toBeUndefined();
    // An unusable write leaves the previous list alone rather than clearing it.
    expect(result.current.fields.variables).toEqual([{ name: 'x', value: '1' }]);
  });
});
