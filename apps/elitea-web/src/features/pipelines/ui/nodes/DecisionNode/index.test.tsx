import { beforeAll, describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue } from '../../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowNode } from '../../../lib/flow-editor/reactFlowTypes';
import { DecisionNode } from './index';

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

function renderDecisionNode(nodeId: string, flowEditorValue: FlowEditorContextValue) {
  const flowNode: FlowNode = { id: nodeId, type: 'testNode', position: { x: 0, y: 0 }, data: {} };

  return renderWithTheme(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[flowNode]}
          edges={[]}
          nodeTypes={{
            testNode: props => (
              <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: nodeId.endsWith('~~~DecisionNode') }}>
                <DecisionNode {...props} />
              </NodeConfigPanelContext.Provider>
            ),
          }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
  );
}

describe('DecisionNode', () => {
  it('renders NormalDecisionNode content for an id that does NOT end with the legacy decision suffix', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Decision 1', nodes: ['NodeA'] }] };
    const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject });

    const { getByText } = renderDecisionNode('Decision 1', flowEditorValue);

    // Both variants render a "Decision outputs" heading via DecisionOutputs,
    // so distinguish by NormalDecisionNode's own "Description" AIAssistantInput
    // label -- LegacyDecisionNode renders the same label too, so instead
    // assert on the presence of the InputSelect's "Input" label, which is
    // unique to NormalDecisionNode (LegacyDecisionNode uses DecisionInputPicker's
    // own "Decision input" heading instead).
    expect(getByText('Decision outputs')).toBeInTheDocument();
    // NormalDecisionNode uses InputSelect (label "Input"), never
    // DecisionInputPicker's own "Decision input" heading -- the discriminator
    // the second test below relies on for the legacy variant.
    expect(() => getByText('Decision input')).toThrow();
  });

  it('renders LegacyDecisionNode content for an id ending with the legacy decision suffix', () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Decision 1', decision: { description: 'legacy desc', decisional_inputs: [], nodes: [] } }],
    };
    const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject });

    const { getByText } = renderDecisionNode('Decision 1~~~DecisionNode', flowEditorValue);

    // LegacyDecisionNode's own DecisionInputPicker renders a "Decision input"
    // heading that NormalDecisionNode never renders (it uses InputSelect
    // instead) -- a reliable discriminator between the two variants.
    expect(getByText('Decision input')).toBeInTheDocument();
  });
});
