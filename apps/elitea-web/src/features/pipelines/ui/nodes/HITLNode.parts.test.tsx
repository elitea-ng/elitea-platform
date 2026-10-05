import type { ReactNode } from 'react';

import { renderHook } from '@testing-library/react';
import { beforeAll, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ReactFlow, ReactFlowProvider, type Edge } from '@xyflow/react';

import { buildFlowEditorContextValue } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeCardContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import {
  HITLRouteRow,
  HITLNodeHandles,
  computeRouteSelectDisabled,
  useHITLNodeModel,
} from './HITLNode.parts';

// `useHITLNodeModel` calls `useEdges()` (`@xyflow/react`), which requires a
// `<ReactFlowProvider>` ancestor -- no actual `<ReactFlow>` mount is needed
// for that hook alone (it never calls `useNodeId()`), so `initialEdges`
// alone is enough to seed the edges array it reads.
function makeWrapper(
  contextValue: FlowEditorContextValue | undefined,
  edges: Edge[] = [],
): ({ children }: { children: ReactNode }) => ReactNode {
  return function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return (
      <ReactFlowProvider initialEdges={edges}>
        {contextValue ? <FlowEditorContext.Provider value={contextValue}>{children}</FlowEditorContext.Provider> : children}
      </ReactFlowProvider>
    );
  };
}

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

describe('computeRouteSelectDisabled', () => {
  it('is disabled when the pipeline is running, regardless of other flags', () => {
    expect(computeRouteSelectDisabled('approve', true, false, true)).toBe(true);
  });

  it('is disabled when the node itself is disabled', () => {
    expect(computeRouteSelectDisabled('reject', false, true, true)).toBe(true);
  });

  it('disables the edit route when canEditRoute is false', () => {
    expect(computeRouteSelectDisabled('edit', false, false, false)).toBe(true);
  });

  it('enables the edit route when canEditRoute is true and nothing else disables it', () => {
    expect(computeRouteSelectDisabled('edit', false, false, true)).toBe(false);
  });

  it('never disables approve/reject on account of canEditRoute', () => {
    expect(computeRouteSelectDisabled('approve', false, false, false)).toBe(false);
    expect(computeRouteSelectDisabled('reject', false, false, false)).toBe(false);
  });
});

describe('HITLNodeHandles', () => {
  it('renders one target handle and one source handle per HITL action, labelled from HITL_ACTIONS', () => {
    const { container, getByText } = renderWithTheme(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{
            testNode: () => (
              <NodeCardContext.Provider value={{ isExpanded: true }}>
                <HITLNodeHandles
                  isRunningPipeline={false}
                  disabled={false}
                  isTargetConnectable
                  isPerforming={false}
                />
              </NodeCardContext.Provider>
            ),
          }}
        />
      </ReactFlowProvider>,
    );

    // 1 target ("hitlNode") + 3 sources (approve/edit/reject).
    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(4);
    expect(getByText('Approve')).toBeInTheDocument();
    expect(getByText('Edit')).toBeInTheDocument();
    expect(getByText('Reject')).toBeInTheDocument();
  });
});

describe('HITLRouteRow', () => {
  const action = { label: 'Approve', chipLabel: 'APPROVE', value: 'approve' as const };

  it('renders the chip label and calls onChange with the selected option value', () => {
    const onChange = vi.fn();
    const { getByText, getByRole } = renderWithTheme(
      <HITLRouteRow
        action={action}
        value=""
        onChange={onChange}
        options={[{ label: 'Tool 1', value: 'Tool 1' }]}
        disabled={false}
        error=""
      />,
    );

    expect(getByText('APPROVE')).toBeInTheDocument();
    const combobox = getByRole('combobox');
    expect(combobox).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('renders the error text and disables the select when instructed', () => {
    const { getByText, getByRole } = renderWithTheme(
      <HITLRouteRow
        action={{ label: 'Edit', chipLabel: 'EDIT', value: 'edit' }}
        value=""
        onChange={vi.fn()}
        options={[]}
        disabled
        error="Provide an edit state key before using the Edit route."
      />,
    );

    expect(getByText('Provide an edit state key before using the Edit route.')).toBeInTheDocument();
    expect(getByRole('combobox')).toHaveAttribute('aria-disabled', 'true');
  });
});

describe('useHITLNodeModel', () => {
  it('falls back to a stable empty FlowEditorContext with no ancestor Provider', () => {
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(undefined),
    });

    expect(result.current.isRunningPipeline).toBe(false);
    expect(result.current.disabled).toBe(false);
    expect(result.current.isEntrypoint).toBe(false);
    expect(result.current.routes).toEqual({});
  });

  it('marks isEntrypoint true only when entry_point matches this node id', () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1' }], entry_point: 'Node1' },
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.isEntrypoint).toBe(true);
  });

  it('isTargetConnectable is false once an edge already targets this node', () => {
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'Node1' }] } });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue, [{ id: 'e1', source: 'X', target: 'Node1' }]),
    });

    expect(result.current.isTargetConnectable).toBe(false);
  });

  it('defaults userMessageType to "fixed" and disables the input select (fstring-only)', () => {
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'Node1' }] } });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.userMessageType).toBe('fixed');
    expect(result.current.userMessageValue).toBe('');
    expect(result.current.inputSelectDisabled).toBe(true);
    expect(result.current.inputSelectTooltipTitle).not.toBe('');
  });

  it('enables the input select once user_message.type is "fstring" and nothing else disables it', () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', user_message: { type: 'fstring', value: '{{x}}' } }] },
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.userMessageType).toBe('fstring');
    expect(result.current.userMessageValue).toBe('{{x}}');
    expect(result.current.inputSelectDisabled).toBe(false);
    expect(result.current.inputSelectTooltipTitle).toBe('');
  });

  it('inputSelectDisabled stays true when fstring but the pipeline is running or the node is disabled', () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', user_message: { type: 'fstring', value: 'x' } }] },
      isRunningPipeline: true,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.inputSelectDisabled).toBe(true);
  });

  it('handleUserMessageMappingChange clears input[] when switching away from fstring', () => {
    const setYamlJsonObject = vi.fn();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', user_message: { type: 'fstring', value: '{{x}}' }, input: ['x'] }] },
      setYamlJsonObject,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    result.current.handleUserMessageMappingChange('user_message', { type: 'fixed', value: 'hello' });

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({
        nodes: [expect.objectContaining({ id: 'Node1', user_message: { type: 'fixed', value: 'hello' }, input: [] })],
      }),
    );
  });

  it('handleUserMessageMappingChange preserves other fields when NOT switching away from fstring', () => {
    const setYamlJsonObject = vi.fn<(next: YamlPipelineDocument) => void>();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', input: ['keepme'] }] },
      setYamlJsonObject,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    result.current.handleUserMessageMappingChange('user_message', { type: 'fstring', value: '{{y}}' });

    const nextDoc = setYamlJsonObject.mock.calls[0]?.[0];
    const node = nextDoc?.nodes?.find(n => n.id === 'Node1');
    expect(node?.['user_message']).toEqual({ type: 'fstring', value: '{{y}}' });
    expect(node?.['input']).toEqual(['keepme']);
  });

  it('handleUserMessageMappingChange defaults a missing next.type to "fixed"', () => {
    const setYamlJsonObject = vi.fn();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      setYamlJsonObject,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    result.current.handleUserMessageMappingChange('user_message', { value: 'no-type' });

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({
        nodes: [expect.objectContaining({ id: 'Node1', user_message: { type: 'fixed', value: 'no-type' } })],
      }),
    );
  });

  it('routes defaults to the shared empty object when the yaml node has no routes field', () => {
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'Node1' }] } });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.routes).toEqual({});
  });

  it('handleRouteSelectChange writes routes[action] onto the node, clears the node-level transition, and adds a new edge with no interrupt label', () => {
    const setYamlJsonObject = vi.fn();
    const setFlowEdges = vi.fn();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', routes: { approve: 'Old' }, transition: 'Old' }] },
      setYamlJsonObject,
      setFlowEdges,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    result.current.handleRouteSelectChange('approve')('NewTarget');

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({
        nodes: [{ id: 'Node1', routes: { approve: 'NewTarget' } }],
      }),
    );

    expect(setFlowEdges).toHaveBeenCalledTimes(1);
    const updater = setFlowEdges.mock.calls[0]?.[0] as (prev: unknown[]) => unknown[];
    const existingEdges = [{ id: 'stale', source: 'Node1', sourceHandle: 'hitlNode_approve', target: 'Old' }];
    const nextEdges = updater(existingEdges);
    expect(nextEdges).toEqual([
      expect.objectContaining({
        id: 'xy-edge__Node1approve---NewTarget',
        source: 'Node1',
        sourceHandle: 'hitlNode_approve',
        target: 'NewTarget',
        type: 'custom',
        data: {},
      }),
    ]);
  });

  it('handleRouteSelectChange marks the new edge as an interrupt when the target is in interrupt_before', () => {
    const setFlowEdges = vi.fn();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1' }], interrupt_before: ['NewTarget'] },
      setFlowEdges,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    result.current.handleRouteSelectChange('reject')('NewTarget');

    const updater = setFlowEdges.mock.calls[0]?.[0] as (prev: unknown[]) => unknown[];
    const nextEdges = updater([]);
    expect(nextEdges).toEqual([expect.objectContaining({ data: { label: 'interrupt' } })]);
  });

  it('handleRouteSelectChange with an empty value only removes the stale edge, adding nothing back', () => {
    const setFlowEdges = vi.fn();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', routes: { edit: 'Old' } }] },
      setFlowEdges,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    result.current.handleRouteSelectChange('edit')('');

    const updater = setFlowEdges.mock.calls[0]?.[0] as (prev: unknown[]) => unknown[];
    const existingEdges = [{ id: 'stale', source: 'Node1', sourceHandle: 'hitlNode_edit', target: 'Old' }];
    expect(updater(existingEdges)).toEqual([]);
  });

  it('synthesises a "not in state" option for an edit_state_key absent from state, sorted first', () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', edit_state_key: 'orphan_key' }], state: { input: { type: 'str' } } },
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.editStateKey).toBe('orphan_key');
    expect(result.current.editStateKeyOptions[0]).toEqual(
      expect.objectContaining({ value: 'orphan_key' }),
    );
  });

  it('does not duplicate the edit_state_key option when it is already a real state key', () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', edit_state_key: 'input' }], state: { input: { type: 'str' } } },
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    const matches = result.current.editStateKeyOptions.filter(option => option.value === 'input');
    expect(matches).toHaveLength(1);
  });

  it('canEditRoute/isEditRouteInvalid: neither an edit route nor a key -> not invalid, cannot edit', () => {
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'Node1' }] } });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.canEditRoute).toBe(false);
    expect(result.current.isEditRouteInvalid).toBe(false);
    expect(result.current.routeErrorText).toBe('');
  });

  it('canEditRoute/isEditRouteInvalid: an edit route configured but no key -> invalid, error text set', () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', routes: { edit: 'Target' } }] },
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.canEditRoute).toBe(true);
    expect(result.current.isEditRouteInvalid).toBe(true);
    expect(result.current.routeErrorText).toBe(result.current.editRouteErrorMessage);
  });

  it('canEditRoute/isEditRouteInvalid: a key present but no edit route configured -> valid, not invalid', () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', edit_state_key: 'some_key' }] },
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.canEditRoute).toBe(true);
    expect(result.current.isEditRouteInvalid).toBe(false);
    expect(result.current.routeErrorText).toBe('');
  });

  it('handleEditStateKeyChange writes edit_state_key onto the matching yaml node', () => {
    const setYamlJsonObject = vi.fn();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      setYamlJsonObject,
    });
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: null }), {
      wrapper: makeWrapper(contextValue),
    });

    result.current.handleEditStateKeyChange('new_key');

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({ nodes: [expect.objectContaining({ id: 'Node1', edit_state_key: 'new_key' })] }),
    );
  });

  it('resolvedLlmSettings passes through the llmSettings argument unchanged', () => {
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'Node1' }] } });
    const settings = { model_name: 'gpt-4', temperature: 0.7, max_tokens: 1000 };
    const { result } = renderHook(() => useHITLNodeModel({ id: 'Node1', llmSettings: settings }), {
      wrapper: makeWrapper(contextValue),
    });

    expect(result.current.resolvedLlmSettings).toEqual(settings);
    expect(result.current.simpleLLMDisabled).toBe(false);
  });
});
