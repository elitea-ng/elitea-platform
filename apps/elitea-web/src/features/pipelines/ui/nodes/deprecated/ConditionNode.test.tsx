import type { ReactNode } from 'react';
import { fireEvent, screen, within } from '@testing-library/react';
import { beforeAll, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import { ConditionNode } from './ConditionNode';

/**
 * `NodeCard` (unit A2e) has landed for real — no `vi.mock` (R-M1 bans it
 * outside `__mocks__/`; tests substitute only the network boundary). This
 * renders the real `NodeCard`/`NodeCardHeader`/`CustomHandle` tree.
 */
beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

function renderConditionNode(
  overrides: Partial<FlowEditorContextValue> = {},
  yamlJsonObject: YamlPipelineDocument = {
    nodes: [
      {
        id: 'condition-1',
        type: 'condition',
        condition: {
          condition_definition: '{{ input }} == "yes"',
          condition_input: ['input'],
          conditional_outputs: ['branch-a', 'branch-b'],
          default_output: 'branch-else',
        },
      },
      { id: 'branch-a', type: 'default' },
    ],
    state: { input: { type: 'str' } },
  },
) {
  const setYamlJsonObject = vi.fn();
  const contextValue: FlowEditorContextValue = {
    yamlJsonObject,
    setYamlJsonObject,
    setFlowNodes: vi.fn(),
    setFlowEdges: vi.fn(),
    ...overrides,
  };

  function TestNode(): ReactNode {
    return (
      <FlowEditorContext.Provider value={contextValue}>
        <ConditionNode id="condition-1" data={{}} />
        <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: true }}>
          <ConditionNode id="condition-1" data={{}} />
        </NodeConfigPanelContext.Provider>
      </FlowEditorContext.Provider>
    );
  }

  const result = renderWithTheme(
    <ReactFlowProvider>
      <ReactFlow
        nodes={[{ id: 'condition-1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
        edges={[{ id: 'edge-1', source: 'condition-1', target: 'branch-a', sourceHandle: 'conditional_outputs' }]}
        nodeTypes={{ testNode: TestNode }}
      />
    </ReactFlowProvider>,
  );

  return { ...result, setYamlJsonObject };
}

describe('ConditionNode', () => {
  it('renders the condition definition value in the AI Assistant input', () => {
    renderConditionNode();
    expect(screen.getByDisplayValue('{{ input }} == "yes"')).toBeInTheDocument();
  });

  it('renders one chip per conditional output', () => {
    renderConditionNode();
    expect(screen.getByText('branch-a')).toBeInTheDocument();
    expect(screen.getByText('branch-b')).toBeInTheDocument();
  });

  it('removes a conditional output and persists the updated condition', () => {
    const { setYamlJsonObject } = renderConditionNode();

    const chip = screen.getByText('branch-b').closest('[class*="MuiChip-root"]') as HTMLElement;
    fireEvent.click(within(chip).getByTestId('condition-output-remove'));

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    const nextNode = nextDoc.nodes?.find(node => node.id === 'condition-1');
    expect(nextNode?.condition).toMatchObject({
      conditional_outputs: ['branch-a'],
    });
  });

  it('shows the "Conditional input" value as a chip', () => {
    renderConditionNode();
    expect(screen.getByText('input')).toBeInTheDocument();
  });

  it('renders the node header name', () => {
    renderConditionNode();
    expect(screen.getByText('condition-1')).toBeInTheDocument();
  });
});
