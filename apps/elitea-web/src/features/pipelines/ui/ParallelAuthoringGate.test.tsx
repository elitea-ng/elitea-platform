import { cleanup, renderHook, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { InitialNodeData } from '../lib/flow-editor/constants/nodeDefaults.constants';
import { PipelineNodeTypes } from '../lib/flow-editor/constants/flowEditor.constants';
import { isCompilerAdmittedNodeType } from '../lib/flow-editor/constants/runtimeContract.constants';
import { canCreateNodeType, getNodeTypeFlags } from '../lib/flow-editor/helpers/flowEditor.helpers';
import { generateNodeIdByType } from '../lib/flow-editor/helpers/nodeIdentity.helpers';
import { AddNodeMenu } from './AddNodeMenu';
import { useFlowEditorNodeOperations } from './useFlowEditorNodeOperations';
import { useFlowEditorNodeTypes } from './useFlowEditorNodeTypes';

afterEach(cleanup);
describe('fixed Parallel is admitted for authoring (Point 5 2a-2)', () => {
  it('registers the card, admits the strict compiler type, and allows creating it like any admitted node', () => {
    const { result } = renderHook(() => useFlowEditorNodeTypes({ versionTools: undefined, llmSettings: null }));
    expect(result.current.nodeTypes).toHaveProperty('parallel');
    expect(isCompilerAdmittedNodeType('parallel')).toBe(true);
    for (const type of [PipelineNodeTypes.Agent, PipelineNodeTypes.Router]) {
      expect(canCreateNodeType('parallel', getNodeTypeFlags(type, undefined, undefined))).toBe(true);
      expect(canCreateNodeType('agent', getNodeTypeFlags(type, undefined, undefined))).toBe(true);
    }
    expect(isCompilerAdmittedNodeType('map')).toBe(false);
    expect(isCompilerAdmittedNodeType('split_out')).toBe(true);
    expect(isCompilerAdmittedNodeType('aggregate')).toBe(true);
  });
  it('offers Parallel in the real catalog beside the ten other authoring choices and the two shaping nodes', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);
    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const entries = within(await screen.findByRole('menu')).getAllByRole('menuitem').map((item) => item.textContent);
    expect(entries).toHaveLength(13);
    expect(entries).toContain('Parallel');
    expect(entries).toContain('Code');
    expect(entries).toContain('Agent');
  });
  it('creates a Parallel node directly through the YAML and the canvas', () => {
    const setYamlJsonObject = vi.fn(); const setFlowNodes = vi.fn(); const setCenter = vi.fn();
    const current = { entry_point: 'existing', nodes: [{ id: 'existing', type: 'agent' }] };
    const { result } = renderHook(() => useFlowEditorNodeOperations({
      flowNodes: [], setFlowNodes, setFlowEdges: vi.fn(), setYamlJsonObject, yamlJsonObjectRef: { current },
      getViewport: () => ({ x: 0, y: 0, zoom: 1 }), setCenter, getZoom: () => 1,
      editorRef: { current: null }, editorWidth: 800, editorHeight: 600,
    }));
    expect(() => result.current.onNodeCreateAtPosition('parallel', { x: 0, y: 0 })).not.toThrow();
    expect(setYamlJsonObject).toHaveBeenCalled();
    expect(setFlowNodes).toHaveBeenCalled();
  });
  it('seeds only strict Parallel fields with explicit empty branch and output choices', () => {
    const seeded = generateNodeIdByType('parallel', []);
    expect(seeded.id).toBe('Parallel_1');
    expect(InitialNodeData['parallel']).toEqual({ branches: [], max_concurrency: 1, wait: 'all', on_error: 'continue', join: 'append', output: [], transition: 'END' });
    expect(seeded).not.toHaveProperty('input');
    expect(seeded).not.toHaveProperty('input_mapping');
    expect(seeded).not.toHaveProperty('reduction');
  });
});
