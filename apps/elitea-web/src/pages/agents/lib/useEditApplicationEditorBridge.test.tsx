import { act, renderHook } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { describe, expect, it } from 'vitest';

import type { ApplicationCreationInput } from '@/entities/application-form';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

import { useEditApplicationEditorBridge } from './useEditApplicationEditorBridge';
import { useEditApplicationVersionFields } from './useEditApplicationVersionFields';

const VERSION = {
  id: '7',
  name: 'base',
  instructions: 'Be helpful.',
  llm_settings: { model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1, temperature: 0.6 },
  meta: { step_limit: 25, internal_tools: [] },
} as unknown as ApplicationVersionDetail;

const PICKED = { model_name: 'gpt-4o', model_project_id: 3, max_tokens: 4096, temperature: 0.2 };

/**
 * Mounts the two hooks the way `EditApplication.tsx` composes them, so the
 * assertions below exercise the real routing rather than a stand-in for it.
 */
function renderBridge(version: ApplicationVersionDetail | undefined) {
  return renderHook(() => {
    const form = useForm<ApplicationCreationInput>({
      defaultValues: { name: 'Agent', description: '', version_details: { conversation_starters: [] } },
    });
    const versionFields = useEditApplicationVersionFields(version);
    return { bridge: useEditApplicationEditorBridge(form, versionFields), versionFields, form };
  });
}

describe('useEditApplicationEditorBridge — llm_settings', () => {
  it("exposes the version's settings on values so the picker can render them", () => {
    const { result } = renderBridge(VERSION);
    expect(result.current.bridge.values.version_details.llm_settings).toEqual({
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
      temperature: 0.6,
    });
  });

  it('leaves llm_settings undefined while the version detail is still in flight', () => {
    const { result } = renderBridge(undefined);
    expect(result.current.bridge.values.version_details.llm_settings).toBeUndefined();
  });

  /*
   * The routing that matters: `llm_settings` is not in
   * `applicationCreationSchema`, so a write that fell through to RHF would be
   * dropped on the floor and the Save payload would carry the old model.
   */
  it('routes a model change to the version-fields hook, never to RHF', () => {
    const { result } = renderBridge(VERSION);

    act(() => {
      result.current.bridge.onFieldChange('version_details.llm_settings', PICKED);
    });

    expect(result.current.versionFields.fields.llmSettings).toEqual(PICKED);
    expect(result.current.bridge.values.version_details.llm_settings).toEqual(PICKED);
    expect(result.current.versionFields.isDirty).toBe(true);
    expect(result.current.form.getValues()).not.toHaveProperty('version_details.llm_settings');
  });

  it('routes a fanned-out per-key write the same way', () => {
    const { result } = renderBridge(VERSION);

    act(() => {
      result.current.bridge.onFieldChange('version_details.llm_settings.max_tokens', 8192);
    });

    expect(result.current.bridge.values.version_details.llm_settings?.max_tokens).toBe(8192);
  });

  it('still routes name through RHF', () => {
    const { result } = renderBridge(VERSION);

    act(() => {
      result.current.bridge.onFieldChange('name', 'Renamed');
    });

    expect(result.current.bridge.values.name).toBe('Renamed');
    expect(result.current.versionFields.isDirty).toBe(false);
  });
});
