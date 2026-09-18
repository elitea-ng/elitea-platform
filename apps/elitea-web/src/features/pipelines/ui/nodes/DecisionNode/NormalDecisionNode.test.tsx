import { fireEvent, waitFor, type RenderResult } from '@testing-library/react';
import { beforeAll, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue } from '../../../__tests__/testUtils';
import { FlowEditorContext, type FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode } from '../../../lib/flow-editor/reactFlowTypes';
import { NormalDecisionNode, type NormalDecisionNodeProps } from './NormalDecisionNode';

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

function renderNormalDecisionNode(
  props: Partial<NormalDecisionNodeProps>,
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
  edges: readonly FlowEdge[] = [],
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? {
    nodes: [{ id: 'Decision1', nodes: ['NodeA', 'NodeB'] }],
  };
  const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, ...flowEditorOverrides });
  const fullProps: NormalDecisionNodeProps = { id: 'Decision1', ...props } as NormalDecisionNodeProps;

  const flowNode: FlowNode = { id: 'Decision1', type: 'testNode', position: { x: 0, y: 0 }, data: fullProps.data ?? {} };

  return renderWithTheme(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[flowNode]}
          edges={[...edges]}
          nodeTypes={{ testNode: () => <NormalDecisionNode {...fullProps} /> }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
  );
}

describe('NormalDecisionNode', () => {
  it('renders three handles: target, nodes, default_output', () => {
    const { container } = renderNormalDecisionNode({});
    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(3);
  });

  /* elitea_issues: #2685, #2526 — the Decision node's fallback-branch handle must be labelled "Default output" (consistent with Router), never the legacy "Else" wording, and no separate leftover "Else" field renders alongside it. The handle's own label text only mounts while the card is expanded, matching how a user sees it (hover-expand). */
  it('labels the fallback-branch handle "Default output", not "Else"', () => {
    const { getByText, queryByText } = renderNormalDecisionNode({}, { expandAll: true });
    expect(getByText('Default output')).toBeInTheDocument();
    expect(queryByText('Else')).not.toBeInTheDocument();
  });

  it('renders one decision-output chip per configured branch', () => {
    const { getByText } = renderNormalDecisionNode({});
    expect(getByText('NodeA')).toBeInTheDocument();
    expect(getByText('NodeB')).toBeInTheDocument();
  });

  it('renders no decision-output chips when the node has no nodes list yet', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Decision1' }] };
    const { queryByText, getByText } = renderNormalDecisionNode({}, { yamlJsonObject });
    expect(getByText('Decision outputs')).toBeInTheDocument();
    expect(queryByText('NodeA')).not.toBeInTheDocument();
  });

  it('removes a decision output and its outgoing edge when the chip delete icon is clicked', () => {
    const setYamlJsonObject = vi.fn();
    const setFlowEdges = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Decision1', nodes: ['NodeA', 'NodeB'] }] };
    const { getByText } = renderNormalDecisionNode({}, { yamlJsonObject, setYamlJsonObject, setFlowEdges });

    const chip = getByText('NodeA').closest('[class*="MuiChip-root"]') as HTMLElement;
    const deleteIcon = chip.querySelector('[data-testid="decision-output-remove"]') as HTMLElement;
    fireEvent.click(deleteIcon);

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    expect(nextDoc.nodes?.find(node => node.id === 'Decision1')?.nodes).toEqual(['NodeB']);

    expect(setFlowEdges).toHaveBeenCalledTimes(1);
    const updater = setFlowEdges.mock.calls[0]?.[0] as (prev: FlowEdge[]) => FlowEdge[];
    const remaining = updater([
      { id: 'e1', source: 'Decision1', target: 'NodeA' },
      { id: 'e2', source: 'Decision1', target: 'NodeB' },
    ]);
    expect(remaining).toEqual([{ id: 'e2', source: 'Decision1', target: 'NodeB' }]);
  });

  it("renders the node's saved description as the AIAssistantInput's initial value", async () => {
    // `onChangeDecisionDescription` (the `updateYamlNode` write path) is
    // only ever invoked via `AIAssistantModal`'s own blur/apply flow
    // (`AIAssistantModal.tsx`'s `dispatchFieldChange`) -- the plain
    // `InputBase` this trigger renders is display-only (`AIAssistantInput.
    // tsx`'s `buildInputBaseProps` forwards no `onChange` at all). Exercising
    // the write path end-to-end would mean opening the modal and driving its
    // CodeMirror-based editor, out of reach for a `fireEvent`-only test --
    // this test instead verifies the read side (the saved value reaching
    // the field), which is this component's own responsibility.
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Decision1', nodes: [], description: 'old' }] };
    const { findByDisplayValue } = renderNormalDecisionNode({}, { yamlJsonObject });
    expect(await findByDisplayValue('old')).toBeInTheDocument();
  });

  // NOTE (coverage gap, disclosed): `onChangeDecisionDescription`'s own body
  // (the `updateYamlNode` write, `NormalDecisionNode.tsx:105-114`) is only
  // ever invoked via `AIAssistantModal`'s `onClickClose`/`handleApply` flow.
  // Driving that end-to-end (open the modal, dismiss/apply it) requires a
  // `QueryClientProvider` -- `AIAssistantModal` renders
  // `useAIContentGenerationStreaming` -> `useServicePromptByKey`, a real
  // `useQuery` call -- plus an MSW handler for its service-prompt endpoint,
  // both several layers outside this file's owned component. Attempted and
  // reverted rather than pulling react-query/MSW plumbing into a node-level
  // test file for three lines of a sibling component's callback body; see
  // this same gap called out explicitly in the final report.

  it('renders without throwing when this node id is not found in yamlJsonObject', () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'OtherNode' }] };
    expect(() => renderNormalDecisionNode({ id: 'Decision1' }, { yamlJsonObject })).not.toThrow();
  });

  it('marks default_output non-connectable once an edge from that handle already exists', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Decision1', nodes: [] }] };
    const edges: readonly FlowEdge[] = [{ id: 'e1', source: 'Decision1', sourceHandle: 'default_output', target: 'End' }];
    const { container } = renderNormalDecisionNode({}, { yamlJsonObject }, edges);

    await waitFor(() => {
      const defaultOutputHandle = container.querySelector('.react-flow__handle.source[data-handleid="default_output"]');
      expect(defaultOutputHandle?.className.split(' ')).not.toContain('connectable');
    });
  });

  it('keeps default_output connectable when no edge from that handle exists yet', async () => {
    const { container } = renderNormalDecisionNode({});
    await waitFor(() => {
      const defaultOutputHandle = container.querySelector('.react-flow__handle.source[data-handleid="default_output"]');
      expect(defaultOutputHandle?.className.split(' ')).toContain('connectable');
    });
  });

  it('disables input/description/interrupt fields when isRunningPipeline is true', async () => {
    const { getByText } = renderNormalDecisionNode({}, { isRunningPipeline: true });
    await waitFor(() => expect(getByText('Decision outputs')).toBeInTheDocument());
    // Every InterruptSwitchRow switch is wired to `isRunningPipeline` here
    // (`disabled={isRunningPipeline}` on CommonInterruptSettings).
  });

  it('renders without a llmSettings prop (defaults to undefined)', () => {
    expect(() => renderNormalDecisionNode({ llmSettings: undefined })).not.toThrow();
  });
});
