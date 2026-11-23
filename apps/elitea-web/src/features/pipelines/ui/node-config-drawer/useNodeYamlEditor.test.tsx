import type { ReactNode } from 'react';
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { buildFlowEditorContextValue } from '../../__tests__/testUtils';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { MAX_NODE_YAML_BYTES, useNodeYamlEditor } from './useNodeYamlEditor';

function makeWrapper(overrides: Partial<FlowEditorContextValue> = {}) {
  const ctx = buildFlowEditorContextValue({
    yamlJsonObject: {
      entry_point: 'node_1',
      nodes: [
        { id: 'node_1', type: 'printer', final_message: 'hello', transition: 'END' },
        { id: 'other_node', type: 'llm' },
      ],
    },
    ...overrides,
  });

  function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    return <FlowEditorContext.Provider value={ctx}>{children}</FlowEditorContext.Provider>;
  }

  return { ctx, Wrapper };
}

describe('useNodeYamlEditor', () => {
  it('serializes the selected node and applies valid edits', () => {
    const setYamlJsonObject = vi.fn();
    const { Wrapper } = makeWrapper({ setYamlJsonObject });
    const { result } = renderHook(() => useNodeYamlEditor({ nodeId: 'node_1' }), { wrapper: Wrapper });

    expect(result.current.code).toContain('id: node_1');
    expect(result.current.error).toBeNull();

    act(() => {
      result.current.onChangeCode('id: node_1\ntype: printer\nfinal_message: updated\ntransition: END\n');
    });

    expect(result.current.error).toBeNull();
    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as { readonly nodes?: readonly { readonly id: string; readonly final_message?: string }[] };
    expect(written.nodes?.find((node) => node.id === 'node_1')?.final_message).toBe('updated');
  });

  it('accepts YAML at exactly MAX_NODE_YAML_BYTES and rejects MAX_NODE_YAML_BYTES + 1', () => {
    const setYamlJsonObject = vi.fn();
    const { Wrapper } = makeWrapper({ setYamlJsonObject });
    const { result } = renderHook(() => useNodeYamlEditor({ nodeId: 'node_1' }), { wrapper: Wrapper });

    const prefix = 'id: node_1\ntype: printer\ntransition: END\nfinal_message: ';
    const exactPayload = `${prefix}${'a'.repeat(MAX_NODE_YAML_BYTES - prefix.length)}`;
    expect(new TextEncoder().encode(exactPayload).length).toBe(MAX_NODE_YAML_BYTES);

    act(() => {
      result.current.onChangeCode(exactPayload);
    });
    expect(result.current.error).toBeNull();
    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);

    const overLimitPayload = `${exactPayload}b`;
    expect(new TextEncoder().encode(overLimitPayload).length).toBe(MAX_NODE_YAML_BYTES + 1);

    act(() => {
      result.current.onChangeCode(overLimitPayload);
    });
    expect(result.current.error).toBe('The node YAML exceeds the maximum allowed size.');
    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
  });

  it('rejects YAML anchors and aliases before parsing', () => {
    const setYamlJsonObject = vi.fn();
    const { Wrapper } = makeWrapper({ setYamlJsonObject });
    const { result } = renderHook(() => useNodeYamlEditor({ nodeId: 'node_1' }), { wrapper: Wrapper });

    act(() => {
      result.current.onChangeCode('id: node_1\ntype: printer\na: &bomb [1, 2]\nb: *bomb\n');
    });

    expect(result.current.error).toBe('YAML anchors and aliases are not supported in node YAML.');
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('rejects non-single-node YAML, missing id, type changes, and duplicate ids', () => {
    const setYamlJsonObject = vi.fn();
    const { Wrapper } = makeWrapper({ setYamlJsonObject });
    const { result } = renderHook(() => useNodeYamlEditor({ nodeId: 'node_1' }), { wrapper: Wrapper });

    act(() => {
      result.current.onChangeCode('- id: node_1\n- id: node_2\n');
    });
    expect(result.current.error).toBe('The node YAML must describe exactly one node.');

    act(() => {
      result.current.onChangeCode('type: printer\n');
    });
    expect(result.current.error).toBe('The node YAML must include a valid id.');

    act(() => {
      result.current.onChangeCode('id: node_1\ntype: llm\n');
    });
    expect(result.current.error).toBe('Changing the node type in YAML view is not supported.');

    act(() => {
      result.current.onChangeCode('id: other_node\ntype: printer\n');
    });
    expect(result.current.error).toContain('used by other nodes');
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('checks setYamlJsonObject === false on rename before mutating flow nodes/edges', () => {
    const setYamlJsonObject = vi.fn(() => false);
    const setFlowNodes = vi.fn();
    const setFlowEdges = vi.fn();
    const onRenamed = vi.fn();
    const { Wrapper } = makeWrapper({ setYamlJsonObject, setFlowNodes, setFlowEdges });
    const { result } = renderHook(() => useNodeYamlEditor({ nodeId: 'node_1', onRenamed }), { wrapper: Wrapper });

    act(() => {
      result.current.onChangeCode('id: renamed_node\ntype: printer\ntransition: END\n');
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    expect(setFlowNodes).not.toHaveBeenCalled();
    expect(setFlowEdges).not.toHaveBeenCalled();
    expect(onRenamed).not.toHaveBeenCalled();
  });

  it('ignores writes when disabled or pipeline is running', () => {
    const setYamlJsonObject = vi.fn();
    const { Wrapper: DisabledWrapper } = makeWrapper({ disabled: true, setYamlJsonObject });
    const disabledHook = renderHook(() => useNodeYamlEditor({ nodeId: 'node_1' }), { wrapper: DisabledWrapper });

    act(() => {
      disabledHook.result.current.onChangeCode('id: node_1\ntype: printer\nfinal_message: nope\n');
    });
    expect(setYamlJsonObject).not.toHaveBeenCalled();

    const { Wrapper: RunningWrapper } = makeWrapper({ isRunningPipeline: true, setYamlJsonObject });
    const runningHook = renderHook(() => useNodeYamlEditor({ nodeId: 'node_1' }), { wrapper: RunningWrapper });

    act(() => {
      runningHook.result.current.onChangeCode('id: node_1\ntype: printer\nfinal_message: nope\n');
    });
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});
