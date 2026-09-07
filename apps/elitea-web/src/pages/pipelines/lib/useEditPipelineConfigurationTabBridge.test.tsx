import { act, renderHook } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { describe, expect, it } from 'vitest';

import type { ApplicationCreationInput } from '@/entities/application-form';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

import { useEditPipelineConfigurationTabBridge } from './useEditPipelineConfigurationTabBridge';
import { useEditPipelineVersionFields } from './useEditPipelineVersionFields';

const VERSION = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  agent_type: 'pipeline',
  instructions: 'entry_point: Printer_1\n',
  welcome_message: 'stored hello',
  conversation_starters: ['Hi there'],
  variables: [{ name: 'x', value: '1' }],
  tools: [{ type: 'toolkit', name: 'github' }],
  llm_settings: { model_name: 'gpt-4o', model_project_id: 9 },
  meta: { icon_meta: { id: 9 }, internal_tools: ['attachments'] },
} as unknown as ApplicationVersionDetail;

function useBridgeUnderTest(version: ApplicationVersionDetail | undefined) {
  const form = useForm<ApplicationCreationInput>({
    values: { name: 'My Pipeline', description: 'A helpful pipeline', version_details: { conversation_starters: ['Hi there'] } },
  });
  const versionFields = useEditPipelineVersionFields(version);
  return { bridge: useEditPipelineConfigurationTabBridge(version, form.setValue, versionFields), form, versionFields };
}

describe('useEditPipelineConfigurationTabBridge', () => {
  it('maps the version onto the shape ConfigurationTab reads', () => {
    const { result } = renderHook(() => useBridgeUnderTest(VERSION));

    const details = result.current.bridge.versionDetails;
    expect(details?.id).toBe('1');
    expect(details?.agent_type).toBe('pipeline');
    expect(details?.instructions).toBe('entry_point: Printer_1\n');
    expect(details?.conversation_starters).toEqual(['Hi there']);
    expect(details?.meta?.icon_meta).toEqual({ id: 9 });
    expect(details?.meta?.internal_tools).toEqual(['attachments']);
  });

  it('returns undefined while the version has not loaded', () => {
    const { result } = renderHook(() => useBridgeUnderTest(undefined));
    expect(result.current.bridge.versionDetails).toBeUndefined();
  });

  /**
   * The regression this bridge was rewritten for. The chat panel writes model
   * settings one key at a time (`version_details.llm_settings.<key>`) while
   * the configuration form's picker writes the whole object; they used to
   * land in two DIFFERENT pieces of state — this module's own override map
   * and `useEditPipelineLlmSettings` — so the last render won and only one of
   * them reached the save body.
   */
  it('routes a per-key llm_settings write into the SAME state the save body reads', () => {
    const { result } = renderHook(() => useBridgeUnderTest(VERSION));

    act(() => {
      result.current.bridge.setFieldValue('version_details.llm_settings.max_tokens', 2048);
    });

    expect(result.current.versionFields.fields.llmSettings?.max_tokens).toBe(2048);
    expect(result.current.bridge.versionDetails?.llm_settings?.max_tokens).toBe(2048);
  });

  it('overlays the live welcome message, variables and modules onto what the editor reads', () => {
    const { result } = renderHook(() => useBridgeUnderTest(VERSION));

    act(() => {
      result.current.bridge.setFieldValue('version_details.welcome_message', 'typed');
      result.current.bridge.setFieldValue('version_details.variables', [{ name: 'y', value: '2' }]);
      result.current.bridge.setFieldValue('version_details.meta.internal_tools', []);
    });

    expect(result.current.bridge.versionDetails?.welcome_message).toBe('typed');
    expect(result.current.bridge.versionDetails?.variables).toEqual([{ name: 'y', value: '2' }]);
    expect(result.current.bridge.versionDetails?.meta?.internal_tools).toEqual([]);
    // The merge is over the stored blob, so an unrelated key survives.
    expect(result.current.bridge.versionDetails?.meta?.icon_meta).toEqual({ id: 9 });
  });

  it('routes name and description into the RHF form', () => {
    const { result } = renderHook(() => useBridgeUnderTest(VERSION));

    act(() => {
      result.current.bridge.setFieldValue('name', 'Renamed');
      result.current.bridge.setFieldValue('description', 'Rewritten');
    });

    expect(result.current.form.getValues().name).toBe('Renamed');
    expect(result.current.form.getValues().description).toBe('Rewritten');
  });

  /**
   * `tools` is an OVERLAY, not saved state: its only writer is
   * `usePipelineMCPToolsStatusMonitor`, which republishes the attached tools
   * with a live `online` flag for display. The version PUT reads no `tools`
   * key at all, so routing it into saved state would invent a dirty flag for
   * something Save cannot persist.
   */
  it('holds a tools write as a display overlay without marking the page dirty', () => {
    const { result } = renderHook(() => useBridgeUnderTest(VERSION));

    act(() => {
      result.current.bridge.setFieldValue('version_details.tools', [{ type: 'toolkit', name: 'github', online: true }]);
    });

    expect(result.current.bridge.versionDetails?.tools).toEqual([{ type: 'toolkit', name: 'github', online: true }]);
    expect(result.current.versionFields.isDirty).toBe(false);
  });

  it('keeps the previous tools overlay when handed something that is not a list', () => {
    const { result } = renderHook(() => useBridgeUnderTest(VERSION));

    act(() => {
      result.current.bridge.setFieldValue('version_details.tools', 'nonsense');
    });

    expect(result.current.bridge.versionDetails?.tools).toEqual([{ type: 'toolkit', name: 'github' }]);
  });
});
