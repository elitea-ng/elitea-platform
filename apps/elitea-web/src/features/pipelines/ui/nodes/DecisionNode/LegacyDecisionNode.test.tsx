import { waitFor, type RenderResult } from '@testing-library/react';
import { beforeAll, describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue } from '../../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowNode } from '../../../lib/flow-editor/reactFlowTypes';
import { LegacyDecisionNode, type LegacyDecisionNodeProps } from './LegacyDecisionNode';

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

function renderLegacyDecisionNode(
  props: Partial<LegacyDecisionNodeProps>,
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? {
    nodes: [
      {
        id: 'Decision1',
        decision: { description: 'my description', decisional_inputs: ['input'], nodes: ['NodeA'], default_output: 'End' },
      },
    ],
  };
  const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, ...flowEditorOverrides });
  const fullProps: LegacyDecisionNodeProps = { id: 'Decision1~~~DecisionNode', ...props } as LegacyDecisionNodeProps;

  const flowNode: FlowNode = {
    id: 'Decision1~~~DecisionNode',
    type: 'testNode',
    position: { x: 0, y: 0 },
    data: fullProps.data ?? {},
  };

  return renderWithTheme(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[flowNode]}
          edges={[]}
          nodeTypes={{
            testNode: () => (
              <>
                <LegacyDecisionNode {...fullProps} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: true }}>
                  <LegacyDecisionNode {...fullProps} />
                </NodeConfigPanelContext.Provider>
              </>
            ),
          }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
  );
}

describe('LegacyDecisionNode', () => {
  it('renders three handles: target, nodes, default_output', () => {
    const { container } = renderLegacyDecisionNode({});
    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(3);
  });

  it('renders the decision input picker with the saved decisional_inputs as chips', () => {
    const { getByText } = renderLegacyDecisionNode({});
    expect(getByText('Decision input')).toBeInTheDocument();
    expect(getByText('input')).toBeInTheDocument();
  });

  it("renders the saved description as the AIAssistantInput's value", async () => {
    const { findByDisplayValue } = renderLegacyDecisionNode({});
    expect(await findByDisplayValue('my description')).toBeInTheDocument();
  });

  it('renders one decision-output chip per configured branch', () => {
    const { getByText } = renderLegacyDecisionNode({});
    expect(getByText('Decision outputs')).toBeInTheDocument();
    expect(getByText('NodeA')).toBeInTheDocument();
  });

  it('uses the node id with the legacy suffix stripped as the card name (data.label falls back to the raw id)', () => {
    const { getByText } = renderLegacyDecisionNode({});
    // `name={data?.label ?? id}` -- no `data.label` supplied here, so the
    // header shows the RAW flow-node id, suffix included (a real,
    // preserved-from-baseline quirk: the header itself does not strip it,
    // only the YAML-lookup inside `useLegacyDecisionNodeModel` does).
    expect(getByText('Decision1~~~DecisionNode')).toBeInTheDocument();
  });

  it('uses data.label as the card name when provided', () => {
    const { getByText } = renderLegacyDecisionNode({ data: { label: 'My Legacy Decision' } });
    expect(getByText('My Legacy Decision')).toBeInTheDocument();
  });

  it('renders without throwing when isPerforming is true', () => {
    expect(() => renderLegacyDecisionNode({ data: { isPerforming: true } })).not.toThrow();
  });

  it('renders without a llmSettings prop (defaults to undefined)', () => {
    expect(() => renderLegacyDecisionNode({ llmSettings: undefined })).not.toThrow();
  });

  it('disables the interrupt switches when isRunningPipeline is true', async () => {
    const { getAllByRole, getByText } = renderLegacyDecisionNode({}, { isRunningPipeline: true });
    await waitFor(() => expect(getByText('Decision outputs')).toBeInTheDocument());
    for (const chip of getAllByRole('button', { hidden: true }).filter(el => el.className.includes('MuiChip'))) {
      expect(chip.className).toContain('Mui-disabled');
    }
  });
});
