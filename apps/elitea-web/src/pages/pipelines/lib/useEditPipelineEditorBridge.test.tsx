import { act, renderHook } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { describe, expect, it } from 'vitest';

import type { ApplicationCreationInput } from '@/entities/application-form';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

import { useEditPipelineEditorBridge } from './useEditPipelineEditorBridge';
import { useEditPipelineVersionFields } from './useEditPipelineVersionFields';

const VERSION = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  agent_type: 'pipeline',
  welcome_message: 'stored hello',
  variables: [{ name: 'x', value: '1' }],
  meta: { step_limit: 40, internal_tools: [] },
} as unknown as ApplicationVersionDetail;

const FORM_VALUES: ApplicationCreationInput = {
  name: 'My Pipeline',
  description: 'A helpful pipeline',
  version_details: { conversation_starters: ['Hi there'] },
};

/** The page's own pairing: the RHF form, the version-level state, and the bridge that presents both to `CreateAgentForm`. */
function useBridgeUnderTest(versionId: number | undefined = 1) {
  const form = useForm<ApplicationCreationInput>({ values: FORM_VALUES });
  const versionFields = useEditPipelineVersionFields(VERSION);
  return { bridge: useEditPipelineEditorBridge(form, versionFields, versionId), form, versionFields };
}

describe('useEditPipelineEditorBridge', () => {
  it('presents the RHF fields and the version-level fields as one values object', () => {
    const { result } = renderHook(() => useBridgeUnderTest());

    expect(result.current.bridge.values).toEqual({
      name: 'My Pipeline',
      description: 'A helpful pipeline',
      version_details: {
        id: 1,
        conversation_starters: ['Hi there'],
        welcome_message: 'stored hello',
        variables: [{ name: 'x', value: '1' }],
        meta: { step_limit: 40 },
        llm_settings: undefined,
      },
    });
  });

  /**
   * `version_details.instructions` is deliberately absent: the form mounts
   * with `showInstructions={false}` because a pipeline's `instructions`
   * column IS its flow-graph YAML, authored on the canvas.
   */
  it('exposes no instructions field', () => {
    const { result } = renderHook(() => useBridgeUnderTest());
    expect(result.current.bridge.values.version_details).not.toHaveProperty('instructions');
  });

  it('routes name and description into the RHF form', () => {
    const { result } = renderHook(() => useBridgeUnderTest());

    act(() => {
      result.current.bridge.onFieldChange('name', 'Renamed');
      result.current.bridge.onFieldChange('description', 'Rewritten');
    });

    expect(result.current.bridge.values.name).toBe('Renamed');
    expect(result.current.bridge.values.description).toBe('Rewritten');
    // Read back off the form itself, not only off the bridge's projection —
    // the save body reads `form.handleSubmit`'s values, not this object.
    expect(result.current.form.getValues().name).toBe('Renamed');
    expect(result.current.form.getValues().description).toBe('Rewritten');
  });

  it('routes conversation starters into the RHF form, dropping non-string entries', () => {
    const { result } = renderHook(() => useBridgeUnderTest());

    act(() => {
      result.current.bridge.onFieldChange('version_details.conversation_starters', ['a', 7, null, 'b']);
    });

    expect(result.current.bridge.values.version_details.conversation_starters).toEqual(['a', 'b']);
  });

  it('routes a version-level field into useEditPipelineVersionFields, not into the form', () => {
    const { result } = renderHook(() => useBridgeUnderTest());

    act(() => {
      result.current.bridge.onFieldChange('version_details.welcome_message', 'typed');
    });

    expect(result.current.bridge.values.version_details.welcome_message).toBe('typed');
    expect(result.current.versionFields.isDirty).toBe(true);
    expect(result.current.form.getValues().name).toBe('My Pipeline');
  });

  it('ignores a path no control on this form emits', () => {
    const { result } = renderHook(() => useBridgeUnderTest());

    act(() => {
      result.current.bridge.onFieldChange('version_details.made_up', 'nonsense');
    });

    expect(result.current.form.getValues()).toEqual(FORM_VALUES);
    expect(result.current.versionFields.isDirty).toBe(false);
  });

  it('coerces a non-string name write to an empty string rather than storing it', () => {
    const { result } = renderHook(() => useBridgeUnderTest());

    act(() => {
      result.current.bridge.onFieldChange('name', 42);
    });

    expect(result.current.bridge.values.name).toBe('');
  });

  /** `WelcomeMessageInput` re-seeds its own local draft off this id, so a version switch has to change it. */
  it('carries the active version id so the welcome-message input can re-seed on a version switch', () => {
    const { result, rerender } = renderHook(({ versionId }) => useBridgeUnderTest(versionId), {
      initialProps: { versionId: 1 },
    });
    expect(result.current.bridge.values.version_details.id).toBe(1);

    rerender({ versionId: 2 });
    expect(result.current.bridge.values.version_details.id).toBe(2);
  });
});
