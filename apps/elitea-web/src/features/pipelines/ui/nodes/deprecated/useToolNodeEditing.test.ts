import { renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../../select/pipelineToolEntry.types';
import { useToolNodeEditing, useToolNodeState } from './useToolNodeEditing';

const noSchemaDerivation = () => '';

describe('useToolNodeState', () => {
  it('resolves toolkit from the yaml node toolkit_name, falling back to tool then id', () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'tool-1', toolkit_name: 'my-github', task: 'do it', tool: 'create_issue' }],
    };
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, [], noSchemaDerivation));
    expect(result.current.toolkit).toBe('my-github');
    expect(result.current.taskValue).toBe('do it');
    expect(result.current.toolValue).toBe('create_issue');
  });

  it('finds the matching versionTools entry by toolkit_name', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', toolkit_name: 'my-github' }] };
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, versionTools, noSchemaDerivation));
    expect(result.current.selectedToolkit).toEqual(versionTools[0]);
  });

  it('finds the matching versionTools entry by its SCHEMA-derived toolkit_name when the entry has no literal toolkit_name field (regression: dropping this pre-derivation hides the Tool sub-select)', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', toolkit_name: 'schema-only-toolkit' }] };
    // No literal `toolkit_name` on this entry -- only derivable via the schema lookup.
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'custom', name: 'schema-only-toolkit-raw' }];
    const getToolkitNameFromSchema = vi.fn((tool: PipelineToolEntry) => (tool.name === 'schema-only-toolkit-raw' ? 'schema-only-toolkit' : ''));
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, versionTools, getToolkitNameFromSchema));
    // Baseline (`ToolNode.jsx:100-112`) `.map()`s in the schema-derived
    // `toolkit_name` before matching, so the matched entry carries it too --
    // not just a reference to the original, toolkit_name-less `versionTools[0]`.
    expect(result.current.selectedToolkit).toEqual({ ...versionTools[0], toolkit_name: 'schema-only-toolkit' });
    expect(getToolkitNameFromSchema).toHaveBeenCalledWith(versionTools[0]);
  });

  it('does not re-derive from schema when the entry already has a literal toolkit_name', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', toolkit_name: 'my-github' }] };
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
    const getToolkitNameFromSchema = vi.fn().mockReturnValue('should-not-be-used');
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, versionTools, getToolkitNameFromSchema));
    expect(result.current.selectedToolkit).toEqual(versionTools[0]);
    expect(getToolkitNameFromSchema).not.toHaveBeenCalled();
  });
});

describe('useToolNodeEditing functionOptions', () => {
  it('is empty when the selected toolkit has no explicit selected_tools', () => {
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: { type: 'github', toolkit_name: 'my-github' },
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject: { nodes: [] },
        setYamlJsonObject: vi.fn(),
      }),
    );
    expect(result.current.functionOptions).toEqual([]);
  });

  it('lists every explicit selected_tools entry, alphabetically, when the toolkit type has no schema-derived restriction', () => {
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: { type: 'github', toolkit_name: 'my-github', settings: { selected_tools: ['list_issues', 'create_issue'] } },
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject: { nodes: [] },
        setYamlJsonObject: vi.fn(),
      }),
    );
    expect(result.current.functionOptions).toEqual([
      { label: 'create_issue', value: 'create_issue' },
      { label: 'list_issues', value: 'list_issues' },
    ]);
  });

  it('passes an empty string to getSelectedTools when the selected toolkit has no type', () => {
    const getSelectedTools = vi.fn().mockReturnValue([]);
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: { settings: { selected_tools: ['create_issue'] } },
        getToolkitNameFromSchema: () => '',
        getSelectedTools,
        yamlJsonObject: { nodes: [] },
        setYamlJsonObject: vi.fn(),
      }),
    );
    expect(result.current.functionOptions).toEqual([{ label: 'create_issue', value: 'create_issue' }]);
    expect(getSelectedTools).toHaveBeenCalledWith('');
  });

  it('filters explicit selected_tools down to the schema-available set when one is known', () => {
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: { type: 'github', toolkit_name: 'my-github', settings: { selected_tools: ['create_issue', 'delete_repo'] } },
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => ['create_issue'],
        yamlJsonObject: { nodes: [] },
        setYamlJsonObject: vi.fn(),
      }),
    );
    expect(result.current.functionOptions).toEqual([{ label: 'create_issue', value: 'create_issue' }]);
  });

  /** #440: a toolkit that declares no `selected_tools` publishes them at run time; the caller reads the catalogue and passes the names in. */
  it('lists the catalogue names, alphabetically, when the toolkit declares no selected_tools', () => {
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: { type: 'openapi', toolkit_name: 'my-api' },
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject: { nodes: [] },
        setYamlJsonObject: vi.fn(),
        dynamicToolNames: ['zeta_op', 'alpha_op'],
      }),
    );
    expect(result.current.functionOptions).toEqual([
      { label: 'alpha_op', value: 'alpha_op' },
      { label: 'zeta_op', value: 'zeta_op' },
    ]);
  });

  /** An explicit selection still wins: the catalogue is the fallback tier, not an override. */
  it('ignores the catalogue names when the toolkit declares its own selected_tools', () => {
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: { type: 'github', toolkit_name: 'my-github', settings: { selected_tools: ['create_issue'] } },
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject: { nodes: [] },
        setYamlJsonObject: vi.fn(),
        dynamicToolNames: ['should_not_appear'],
      }),
    );
    expect(result.current.functionOptions).toEqual([{ label: 'create_issue', value: 'create_issue' }]);
  });
});

describe('useToolNodeEditing onSelectToolkit', () => {
  it('writes toolkit_name for a non-application toolkit and clears tool', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => 'derived-name',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject,
      }),
    );

    result.current.onSelectToolkit({ type: 'github', toolkit_name: 'my-github' });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ toolkit_name: 'my-github', tool: undefined });
  });

  it('falls back to getToolkitNameFromSchema when the picked non-application toolkit has no toolkit_name', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => 'derived-name',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject,
      }),
    );

    result.current.onSelectToolkit({ type: 'github' });

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ toolkit_name: 'derived-name', tool: undefined });
  });

  it('falls back to getToolkitNameFromSchema when the picked non-application toolkit has an EMPTY STRING toolkit_name (regression: `??` would keep the empty string instead of falling through)', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => 'derived-name',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject,
      }),
    );

    result.current.onSelectToolkit({ type: 'github', toolkit_name: '' });

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ toolkit_name: 'derived-name', tool: undefined });
  });

  it('writes tool (not toolkit_name) for an application-type association', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject,
      }),
    );

    result.current.onSelectToolkit({ type: 'application', name: 'sub-agent' });

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ toolkit_name: undefined, tool: 'sub-agent' });
  });

  it('clears both fields when passed null', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', toolkit_name: 'my-github' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject,
      }),
    );

    result.current.onSelectToolkit(null);

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ toolkit_name: undefined, tool: undefined });
  });

  it('is a no-op when yamlJsonObject is undefined', () => {
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject: undefined,
        setYamlJsonObject,
      }),
    );

    result.current.onSelectToolkit({ type: 'github', toolkit_name: 'my-github' });

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});

describe('useToolNodeEditing handleSetTask', () => {
  it('persists the new task via updateYamlNode', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', task: 'old' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject,
      }),
    );

    result.current.handleSetTask({ target: { value: 'new task' } } as unknown as React.ChangeEvent<HTMLInputElement>);

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ task: 'new task' });
  });

  it('is a no-op when setYamlJsonObject is undefined', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', task: 'old' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject: undefined,
      }),
    );

    expect(() =>
      result.current.handleSetTask({ target: { value: 'new task' } } as unknown as React.ChangeEvent<HTMLInputElement>),
    ).not.toThrow();
  });
});

describe('useToolNodeEditing handleSetTool', () => {
  it('writes the tool field via batchUpdateYamlNode', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1' }] };
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject,
        setYamlJsonObject,
      }),
    );

    result.current.handleSetTool('create_issue');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ tool: 'create_issue' });
  });

  it('is a no-op when yamlJsonObject is undefined', () => {
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() =>
      useToolNodeEditing({
        id: 'tool-1',
        selectedToolkit: undefined,
        getToolkitNameFromSchema: () => '',
        getSelectedTools: () => [],
        yamlJsonObject: undefined,
        setYamlJsonObject,
      }),
    );

    result.current.handleSetTool('create_issue');

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});

describe('useToolNodeState toolkit fallback', () => {
  it('falls back to the node id when neither toolkit_name nor tool is set', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1' }] };
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, [], noSchemaDerivation));
    expect(result.current.toolkit).toBe('tool-1');
  });

  it('falls back to tool when toolkit_name is absent', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', tool: 'create_issue' }] };
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, [], noSchemaDerivation));
    expect(result.current.toolkit).toBe('create_issue');
  });

  it('finds the matching versionTools entry by name when toolkit_name is absent on the entry', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1' }] };
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'application', name: 'tool-1' }];
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, versionTools, noSchemaDerivation));
    // Baseline (`ToolNode.jsx:100-112`) `.map()`s a schema-derived
    // `toolkit_name` onto every entry before matching -- here that's `''`
    // (no schema configured), so the matched entry carries that too.
    expect(result.current.selectedToolkit).toEqual({ ...versionTools[0], toolkit_name: '' });
  });

  it('falls back to tool then id when toolkit_name is an empty string (regression: `??` would keep the empty string instead of falling through)', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', toolkit_name: '', tool: 'create_issue' }] };
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, [], noSchemaDerivation));
    expect(result.current.toolkit).toBe('create_issue');
  });

  it('falls back to id when both toolkit_name and tool are empty strings', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', toolkit_name: '', tool: '' }] };
    const { result } = renderHook(() => useToolNodeState('tool-1', yamlJsonObject, [], noSchemaDerivation));
    expect(result.current.toolkit).toBe('tool-1');
  });
});
