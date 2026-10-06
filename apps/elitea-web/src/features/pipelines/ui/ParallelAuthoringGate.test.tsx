import { cleanup, renderHook, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { InitialNodeData } from '../lib/flow-editor/constants/nodeDefaults.constants';
import { PipelineNodeTypes } from '../lib/flow-editor/constants/flowEditor.constants';
import { FIXED_PARALLEL_AUTHORING_ENABLED } from '../lib/flow-editor/constants/parallel.constants';
import { isCompilerAdmittedNodeType } from '../lib/flow-editor/constants/runtimeContract.constants';
import { canCreateNodeType, getNodeTypeFlags } from '../lib/flow-editor/helpers/flowEditor.helpers';
import { generateNodeIdByType } from '../lib/flow-editor/helpers/nodeIdentity.helpers';
import { AddNodeMenu } from './AddNodeMenu';
import { useFlowEditorNodeOperations } from './useFlowEditorNodeOperations';
import { useFlowEditorNodeTypes } from './useFlowEditorNodeTypes';

afterEach(cleanup);
describe('fixed Parallel parse support is separate from authoring readiness', () => {
  it('registers the stored card, admits the strict compiler type, and leaves the authoring gate false', () => {
    const { result } = renderHook(() => useFlowEditorNodeTypes({ versionTools: undefined, llmSettings: null }));
    expect(result.current.nodeTypes).toHaveProperty('parallel');
    expect(isCompilerAdmittedNodeType('parallel')).toBe(true);
    expect(FIXED_PARALLEL_AUTHORING_ENABLED).toBe(false);
    for (const type of [PipelineNodeTypes.Agent, PipelineNodeTypes.Router]) {
      expect(canCreateNodeType('parallel', getNodeTypeFlags(type, undefined, undefined))).toBe(false);
      expect(canCreateNodeType('agent', getNodeTypeFlags(type, undefined, undefined))).toBe(true);
    }
    expect(isCompilerAdmittedNodeType('map')).toBe(false);
    expect(isCompilerAdmittedNodeType('split_out')).toBe(false);
    expect(isCompilerAdmittedNodeType('aggregate')).toBe(false);
  });
  it('withholds Parallel from the real catalog while retaining the current ten authoring choices', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);
    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const entries = within(await screen.findByRole('menu')).getAllByRole('menuitem').map((item) => item.textContent);
    expect(entries).toHaveLength(10);
    expect(entries).not.toContain('Parallel');
    expect(entries).toContain('Code');
    expect(entries).toContain('Agent');
  });
  it('refuses direct node creation before any YAML, canvas, or reveal mutation', () => {
    const setYamlJsonObject = vi.fn(); const setFlowNodes = vi.fn(); const setCenter = vi.fn();
    const current = { entry_point: 'existing', nodes: [{ id: 'existing', type: 'agent' }] };
    const { result } = renderHook(() => useFlowEditorNodeOperations({
      flowNodes: [], setFlowNodes, setFlowEdges: vi.fn(), setYamlJsonObject, yamlJsonObjectRef: { current },
      getViewport: () => ({ x: 0, y: 0, zoom: 1 }), setCenter, getZoom: () => 1,
      editorRef: { current: null }, editorWidth: 800, editorHeight: 600,
    }));
    expect(() => result.current.onNodeCreateAtPosition('parallel', { x: 0, y: 0 })).toThrow('not enabled');
    expect(() => result.current.onAddNode('parallel')).toThrow('not enabled');
    expect(setYamlJsonObject).not.toHaveBeenCalled();
    expect(setFlowNodes).not.toHaveBeenCalled();
    expect(setCenter).not.toHaveBeenCalled();
    expect(current.nodes).toEqual([{ id: 'existing', type: 'agent' }]);
  });
  it('seeds only strict Parallel fields with explicit empty branch and output choices', () => {
    const seeded = generateNodeIdByType('parallel', []);
    expect(seeded.id).toBe('Parallel_1');
    expect(InitialNodeData['parallel']).toEqual({ branches: [], max_concurrency: 1, wait: 'all', error_policy: 'fail_after_drain', output: [], transition: 'END' });
    expect(seeded).not.toHaveProperty('input');
    expect(seeded).not.toHaveProperty('input_mapping');
    expect(seeded).not.toHaveProperty('reduction');
  });
});
