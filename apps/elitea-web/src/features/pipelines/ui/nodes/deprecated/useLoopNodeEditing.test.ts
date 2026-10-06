import { renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../../select/pipelineToolEntry.types';
import { useLoopNodeEditing } from './useLoopNodeEditing';

const versionTools: readonly PipelineToolEntry[] = [
  { type: 'github', toolkit_name: 'my-github' },
  { type: 'application', name: 'sub-agent' },
];

describe('useLoopNodeEditing yamlNode/taskValue derivation', () => {
  it('finds the matching yaml node by id and exposes its task', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1', task: 'do it' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, vi.fn(), [], () => ''));
    expect(result.current.yamlNode?.id).toBe('loop-1');
    expect(result.current.taskValue).toBe('do it');
  });

  it('defaults taskValue to an empty string when the node has no task', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, vi.fn(), [], () => ''));
    expect(result.current.taskValue).toBe('');
  });

  it('yields an undefined yamlNode when no node matches the id', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'other' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, vi.fn(), [], () => ''));
    expect(result.current.yamlNode).toBeUndefined();
  });
});

describe('useLoopNodeEditing handleSetTask', () => {
  it('persists the new task via updateYamlNode', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1', task: 'old' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, setYamlJsonObject, [], () => ''));

    result.current.handleSetTask({ target: { value: 'new task' } } as unknown as React.ChangeEvent<HTMLInputElement>);

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ task: 'new task' });
  });

  it('is a no-op when setYamlJsonObject is undefined', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1', task: 'old' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, undefined, [], () => ''));

    expect(() => result.current.handleSetTask({ target: { value: 'new task' } } as unknown as React.ChangeEvent<HTMLInputElement>)).not.toThrow();
  });

  it('is a no-op when yamlJsonObject is undefined', () => {
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', undefined, setYamlJsonObject, [], () => ''));

    result.current.handleSetTask({ target: { value: 'new task' } } as unknown as React.ChangeEvent<HTMLInputElement>);

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});

describe('useLoopNodeEditing onChangeToolkit', () => {
  it('clears both toolkit_name and tool when passed null', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1', toolkit_name: 'my-github' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? ''));

    result.current.onChangeToolkit(null);

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-1' });
  });

  it('writes toolkit_name for a non-application toolkit found by toolkit_name', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? ''));

    result.current.onChangeToolkit('my-github');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-1', toolkit_name: 'my-github' });
  });

  it('writes tool (not toolkit_name) for an application-type entry matched by name', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, setYamlJsonObject, versionTools, tool => tool.type ?? ''));

    result.current.onChangeToolkit('sub-agent');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-1', tool: 'sub-agent' });
  });

  it('falls back to matching by getToolkitNameFromSchema when neither toolkit_name nor name match', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1' }] };
    const schemaTools: readonly PipelineToolEntry[] = [{ type: 'jira' }];
    const { result } = renderHook(() =>
      useLoopNodeEditing('loop-1', yamlJsonObject, setYamlJsonObject, schemaTools, tool => `schema-${tool.type}`),
    );

    result.current.onChangeToolkit('schema-jira');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toEqual({ id: 'loop-1', toolkit_name: 'schema-jira' });
  });

  it('is a no-op when yamlJsonObject is undefined', () => {
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', undefined, setYamlJsonObject, versionTools, tool => tool.type ?? ''));

    result.current.onChangeToolkit('my-github');

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});

describe('useLoopNodeEditing onChangeTool', () => {
  it('persists the new tool value via updateYamlNode', () => {
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, setYamlJsonObject, [], () => ''));

    result.current.onChangeTool('create_issue');

    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).toMatchObject({ tool: 'create_issue' });
  });

  it('is a no-op when setYamlJsonObject is undefined', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'loop-1' }] };
    const { result } = renderHook(() => useLoopNodeEditing('loop-1', yamlJsonObject, undefined, [], () => ''));

    expect(() => result.current.onChangeTool('create_issue')).not.toThrow();
  });
});
