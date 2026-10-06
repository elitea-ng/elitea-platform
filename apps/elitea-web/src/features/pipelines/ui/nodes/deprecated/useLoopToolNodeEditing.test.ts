import { renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../../select/pipelineToolEntry.types';
import { useLoopToolNodeEditing } from './useLoopToolNodeEditing';

const versionTools: readonly PipelineToolEntry[] = [
  { type: 'github', toolkit_name: 'my-github' },
  { type: 'application', name: 'sub-agent' },
];

describe('useLoopToolNodeEditing yamlNode/taskValue derivation', () => {
  it('finds the matching yaml node by id and exposes its task', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1', task: 'do it' }] };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, vi.fn(), [], () => '', undefined));
    expect(result.current.yamlNode?.id).toBe('loop-tool-1');
    expect(result.current.taskValue).toBe('do it');
  });

  it('defaults taskValue to an empty string when the node has no task', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, vi.fn(), [], () => '', undefined));
    expect(result.current.taskValue).toBe('');
  });
});

describe('useLoopToolNodeEditing handleSetTask', () => {
  it('persists the new task via updateYamlNode', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1', task: 'old' }] };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, [], () => '', undefined));

    result.current.handleSetTask({ target: { value: 'new task' } } as unknown as React.ChangeEvent<HTMLInputElement>);

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ task: 'new task' });
  });

  it('is a no-op when setYamlJsonObject is undefined', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1', task: 'old' }] };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, undefined, [], () => '', undefined));

    expect(() => result.current.handleSetTask({ target: { value: 'new task' } } as unknown as React.ChangeEvent<HTMLInputElement>)).not.toThrow();
  });
});

describe('useLoopToolNodeEditing onChangeToolkit', () => {
  it('clears toolkit_name and tool when passed null', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1', toolkit_name: 'my-github' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeToolkit(null);

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-tool-1' });
  });

  it('writes toolkit_name for a non-application toolkit', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeToolkit('my-github');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-tool-1', toolkit_name: 'my-github' });
  });

  it('writes tool for an application-type entry', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeToolkit('sub-agent');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-tool-1', tool: 'sub-agent' });
  });

  it('is a no-op when yamlJsonObject is undefined', () => {
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', undefined, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeToolkit('my-github');

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('falls back to matching by getToolkitNameFromSchema when neither toolkit_name nor name match', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const schemaTools: readonly PipelineToolEntry[] = [{ type: 'jira' }];
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, schemaTools, tool => `schema-${tool.type}`, undefined),
    );

    result.current.onChangeToolkit('schema-jira');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-tool-1', toolkit_name: 'schema-jira' });
  });
});

describe('useLoopToolNodeEditing onChangeTool', () => {
  it('persists the new tool value via updateYamlNode', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, [], () => '', undefined));

    result.current.onChangeTool('create_issue');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ tool: 'create_issue' });
  });

  it('is a no-op when setYamlJsonObject is undefined', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, undefined, [], () => '', undefined));

    expect(() => result.current.onChangeTool('create_issue')).not.toThrow();
  });
});

describe('useLoopToolNodeEditing onChangeLoopToolkit', () => {
  it('clears loop_toolkit_name/loop_tool/variables_mapping when passed null', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'loop-tool-1', loop_toolkit_name: 'my-github', loop_tool: 'create_issue', variables_mapping: { a: 'b' } }],
    };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeLoopToolkit(null);

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-tool-1' });
  });

  it('writes loop_toolkit_name (non-application) and derives a default variables_mapping', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeLoopToolkit('my-github');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ loop_toolkit_name: 'my-github' });
    expect(nextDoc.nodes?.[0]).not.toHaveProperty('loop_tool');
    expect(nextDoc.nodes?.[0]?.variables_mapping).toBeDefined();
  });

  it('writes loop_tool (not loop_toolkit_name) for an application-type entry', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeLoopToolkit('sub-agent');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ loop_tool: 'sub-agent' });
    expect(nextDoc.nodes?.[0]).not.toHaveProperty('loop_toolkit_name');
  });

  it('is a no-op when the yamlNode cannot be found', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'other' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeLoopToolkit('my-github');

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});

describe('useLoopToolNodeEditing onChangeLoopTool', () => {
  it('resolves the loop toolkit from loop_toolkit_name and persists the new loop_tool plus derived mapping', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-tool-1', loop_toolkit_name: 'my-github' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeLoopTool('create_issue');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ loop_tool: 'create_issue' });
  });

  it('is a no-op when the yamlNode cannot be found', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'other' }] };
    const { result } = renderHook(() =>
      useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? '', undefined),
    );

    result.current.onChangeLoopTool('create_issue');

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});

describe('useLoopToolNodeEditing onChangeMapping', () => {
  it('merges a new mapping entry into the existing variables_mapping', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'loop-tool-1', variables_mapping: { output: { type: 'fixed', value: 'hello' } } }],
    };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, [], () => '', undefined));

    result.current.onChangeMapping('input', { type: 'fixed', value: 'world' });

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]?.variables_mapping).toEqual({
      output: { type: 'fixed', value: 'hello' },
      input: { type: 'fixed', value: 'world' },
    });
  });

  it('is a no-op when the yamlNode cannot be found', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'other' }] };
    const { result } = renderHook(() => useLoopToolNodeEditing('loop-tool-1', yamlJsonObject, setYamlJsonObject, [], () => '', undefined));

    result.current.onChangeMapping('input', { type: 'fixed', value: 'world' });

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});
