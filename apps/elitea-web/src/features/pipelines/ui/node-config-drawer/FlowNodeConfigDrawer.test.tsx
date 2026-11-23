import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ReactFlowProvider } from '@xyflow/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { buildFlowEditorContextValue } from '../../__tests__/testUtils';
import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { getNodeDocsLink } from '../../lib/flow-editor/constants/nodeDocs.constants';
import { getNodeFieldInfo } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { isCompilerLegalNodeId } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { collectGraphAdmissionIssues } from '../../lib/graphAdmission.helpers';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { getNodeCardV2Summary } from '../node-card-v2/nodeCardV2.helpers';
import { NodeCard } from '../nodes/BaseNode/NodeCard';
import {
  buildDuplicateNodeId,
  cloneYamlNodeForDuplicate,
  duplicatePipelineNode,
} from './duplicateNode.helpers';
import { FlowNodeConfigDrawer } from './FlowNodeConfigDrawer';
import { NodeDeprecationBanner } from './NodeDeprecationBanner';

const { PipelineNodeTypes } = FlowEditorConstants;

function StubNode({ id, type }: { readonly id: string; readonly type?: string }) {
  return (
    <NodeCard id={id} name={id} type={type ?? PipelineNodeTypes.LLM} isEntrypoint={false}>
      <div data-testid="stub-node-form-body">form-body-{id}</div>
    </NodeCard>
  );
}

const TEST_NODE_TYPES = {
  [PipelineNodeTypes.LLM]: StubNode,
  [PipelineNodeTypes.Tool]: StubNode,
  [PipelineNodeTypes.Parallel]: StubNode,
  [PipelineNodeTypes.Map]: StubNode,
};

function makeFlowNode(id: string, type: string = PipelineNodeTypes.LLM): FlowNode {
  return {
    id,
    type,
    position: { x: 100, y: 200 },
    data: { label: id },
    selected: true,
  };
}

function renderDrawer(
  selectedNodeId: string | null,
  flowNodes: readonly FlowNode[],
  contextOverrides: Partial<FlowEditorContextValue> = {},
  callbacks: { onClose?: () => void; onSelectNode?: (id: string | null) => void } = {},
) {
  const flowContext = buildFlowEditorContextValue({
    yamlJsonObject: {
      entry_point: 'node_1',
      nodes: [{ id: 'node_1', type: PipelineNodeTypes.LLM }],
    },
    ...contextOverrides,
  });

  return {
    flowContext,
    ...renderWithTheme(
      <ReactFlowProvider>
        <FlowEditorContext.Provider value={flowContext}>
          <FlowNodeConfigDrawer
            flowNodes={flowNodes}
            nodeTypes={TEST_NODE_TYPES}
            selectedNodeId={selectedNodeId}
            onClose={callbacks.onClose ?? vi.fn()}
            onSelectNode={callbacks.onSelectNode ?? vi.fn()}
          />
        </FlowEditorContext.Provider>
      </ReactFlowProvider>,
    ),
  };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('duplicateNode.helpers', () => {
  it('mints unique _copy suffixes, enforces 128-byte legal ids, and avoids toolNames collisions', () => {
    expect(buildDuplicateNodeId('llm_1', ['llm_1'])).toBe('llm_1_copy');
    expect(buildDuplicateNodeId('llm_1', ['llm_1', 'llm_1_copy'])).toBe('llm_1_copy_2');
    expect(buildDuplicateNodeId('llm_1', ['llm_1'], ['llm_1_copy'])).toBe('llm_1_copy_2');

    const longId = 'a'.repeat(140);
    const minted = buildDuplicateNodeId(longId, [longId]);
    expect(new TextEncoder().encode(minted).length).toBeLessThanOrEqual(128);
    expect(isCompilerLegalNodeId(minted)).toBe(true);
  });

  it.each<[string, YamlPipelineNode]>([
    ['printer', { id: 'p1', type: 'printer', final_message: 'hi', transition: 'next' }],
    ['decision', { id: 'd1', type: 'decision', description: 'pick', nodes: ['a'], default_output: 'b' }],
    ['hitl', { id: 'h1', type: 'hitl', user_message: { type: 'fixed', value: 'ok?' }, routes: { approve: 'a', edit: 'b', reject: 'c' }, edit_state_key: 'k' }],
    ['router', { id: 'r1', type: 'router', condition: 'true' as never, routes: ['a'], default_output: 'b' }],
    ['llm', { id: 'l1', type: 'llm', transition: 'END' }],
    ['code', { id: 'c1', type: 'code', code: { type: 'fixed', value: 'result = 1' }, transition: 'END' }],
    ['state_modifier', { id: 's1', type: 'state_modifier', template: '{{ input }}', transition: 'END' }],
  ])('duplicated %s node passes graph admission with 0 issues', (_label, source) => {
    const dup = cloneYamlNodeForDuplicate(source, `${source.id}_copy`);
    const issues = collectGraphAdmissionIssues({
      entry_point: dup.id,
      nodes: [dup],
    });
    expect(issues).toEqual([]);
  });

  it('clones YAML node and strips outgoing edges and error_transition while keeping other settings', () => {
    const cloned = cloneYamlNodeForDuplicate(
      {
        id: 'code_1',
        type: 'code',
        transition: 'END',
        default_output: 'next_node',
        routes: ['a', 'b'],
        nodes: ['c'],
        settings: { on_error: 'error_output', error_transition: 'err_node', execute_once: true },
      },
      'code_1_copy',
    );
    expect(cloned.id).toBe('code_1_copy');
    expect(cloned.transition).toBeUndefined();
    expect(cloned.default_output).toBeUndefined();
    expect(cloned.routes).toBeUndefined();
    expect(cloned.nodes).toBeUndefined();
    expect(cloned['settings']).toEqual({ on_error: 'error_output', execute_once: true });
  });

  it('duplicates both YAML node and canvas node with offset position', () => {
    const setYamlJsonObject = vi.fn();
    const setFlowNodes = vi.fn();
    const nextId = duplicatePipelineNode({
      sourceId: 'node_1',
      yamlJsonObject: { nodes: [{ id: 'node_1', type: 'llm', transition: 'END' }] },
      flowNodes: [makeFlowNode('node_1')],
      setYamlJsonObject,
      setFlowNodes,
    });
    expect(nextId).toBe('node_1_copy');
    expect(setYamlJsonObject).toHaveBeenCalledWith({
      nodes: [
        { id: 'node_1', type: 'llm', transition: 'END' },
        { id: 'node_1_copy', type: 'llm' },
      ],
    });
    expect(setFlowNodes).toHaveBeenCalledTimes(1);
  });
});

describe('FlowNodeConfigDrawer', () => {
  it('stays closed for null, End, and Ghost selections', () => {
    renderDrawer(null, [makeFlowNode('node_1')]);
    expect(screen.queryByTestId('pipeline-node-panel')).not.toBeInTheDocument();

    renderDrawer('END', [makeFlowNode('END', PipelineNodeTypes.End)]);
    expect(screen.queryByTestId('pipeline-node-panel')).not.toBeInTheDocument();

    renderDrawer('ghost_1', [makeFlowNode('ghost_1', PipelineNodeTypes.Ghost)]);
    expect(screen.queryByTestId('pipeline-node-panel')).not.toBeInTheDocument();
  });

  it('renders the selected node in panel mode with Name field, form body, and footer actions', () => {
    const onSelectNode = vi.fn();
    const handleDeleteNode = vi.fn();
    const { flowContext } = renderDrawer(
      'node_1',
      [makeFlowNode('node_1')],
      { handleDeleteNode },
      { onSelectNode },
    );

    expect(screen.getByTestId('pipeline-node-panel')).toHaveAttribute('data-node-id', 'node_1');
    expect(screen.getByTestId('pipeline-node-panel-name-input')).toHaveValue('node_1');
    expect(screen.getByTestId('stub-node-form-body')).toHaveTextContent('form-body-node_1');
    expect(screen.getByTestId('pipeline-node-panel-entrypoint-state')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-node-panel-build-with-ai')).toBeDisabled();

    fireEvent.click(screen.getByTestId('pipeline-node-panel-duplicate'));
    expect(flowContext.setYamlJsonObject).toHaveBeenCalled();
    expect(onSelectNode).toHaveBeenCalledWith('node_1_copy');

    fireEvent.click(screen.getByTestId('pipeline-node-panel-delete'));
    expect(handleDeleteNode).toHaveBeenCalledWith('node_1');
  });

  it('validates and commits node rename from NodeNameField', async () => {
    const user = userEvent.setup();
    const onSelectNode = vi.fn();
    const { flowContext } = renderDrawer(
      'node_1',
      [makeFlowNode('node_1')],
      {},
      { onSelectNode },
    );

    const input = screen.getByTestId('pipeline-node-panel-name-input');
    await user.clear(input);
    await user.type(input, 'bad name with spaces');
    expect(screen.getByText(/Only letters, numbers/)).toBeInTheDocument();

    await user.clear(input);
    await user.type(input, 'renamed_llm');
    fireEvent.blur(input);

    await waitFor(() => {
      expect(flowContext.setYamlJsonObject).toHaveBeenCalledWith(
        expect.objectContaining({ entry_point: 'renamed_llm' }),
      );
      expect(onSelectNode).toHaveBeenCalledWith('renamed_llm');
    });
  });

  it('shows deprecation banner and disables duplicate for deprecated nodes', () => {
    renderDrawer(
      'legacy_tool',
      [makeFlowNode('legacy_tool', PipelineNodeTypes.Tool)],
      {
        yamlJsonObject: {
          nodes: [{ id: 'legacy_tool', type: PipelineNodeTypes.Tool }],
        },
      },
    );

    expect(screen.getByTestId('pipeline-node-panel-deprecation-banner')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-node-panel-name-input')).toBeDisabled();
    expect(screen.getByTestId('pipeline-node-panel-duplicate')).toBeDisabled();
  });

  it('switches to per-node YAML view', async () => {
    const user = userEvent.setup();
    renderDrawer('node_1', [makeFlowNode('node_1')]);

    const buttons = screen.getByTestId('pipeline-node-panel-view-toggle').querySelectorAll('button');
    await user.click(buttons[1]!);

    expect(screen.getByTestId('pipeline-node-panel-yaml')).toBeInTheDocument();
  });

  it('disables duplicate for Parallel and Map ownership nodes', () => {
    renderDrawer(
      'par_1',
      [makeFlowNode('par_1', PipelineNodeTypes.Parallel)],
      { yamlJsonObject: { nodes: [{ id: 'par_1', type: PipelineNodeTypes.Parallel }] } },
    );
    expect(screen.getByTestId('pipeline-node-panel-duplicate')).toBeDisabled();
  });

  it.each(['__proto__', 'constructor', 'prototype'])('guards prototype key %s safely across drawer, docs, summary, and field info', (protoKey) => {
    renderDrawer(
      'bad_node',
      [makeFlowNode('bad_node', protoKey)],
      { yamlJsonObject: { nodes: [{ id: 'bad_node', type: protoKey }] } },
    );
    expect(screen.queryByTestId('pipeline-node-panel')).not.toBeInTheDocument();

    const { container } = renderWithTheme(<NodeDeprecationBanner type={protoKey} />);
    expect(container.textContent).toBe('');
    expect(getNodeDocsLink(protoKey)).toBeNull();
    expect(getNodeCardV2Summary({ id: 'n', type: protoKey }, { type: protoKey })).toEqual({ primary: '', secondary: '' });
    expect(getNodeFieldInfo(protoKey, protoKey)).toBe('');
  });
});
