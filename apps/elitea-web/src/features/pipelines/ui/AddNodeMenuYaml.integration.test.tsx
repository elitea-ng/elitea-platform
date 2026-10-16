import { renderHook, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { load } from 'js-yaml';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { InitialNodeData } from '../lib/flow-editor/constants/nodeDefaults.constants';
import { createPipelineYamlStore } from '../model/pipelineYamlStore';
import { AddNodeMenu } from './AddNodeMenu';
import { useFlowEditorNodeOperations } from './useFlowEditorNodeOperations';

describe.each([
  { kind: 'empty', source: '' },
  { kind: 'whitespace', source: ' \n' },
  { kind: 'comment only', source: '# original comment\n' },
])('Add node from $kind source through the real YAML store', ({ source }) => {
  it.each([
    { label: 'LLM', type: 'llm', id: 'LLM_1' },
    { label: 'Agent', type: 'agent', id: 'Agent_1' },
  ])('stores the first $label node and closes its menu', async ({ label, type, id }) => {
    const store = createPipelineYamlStore();
    store.getState().initPipelineYaml({ yamlCode: source, yamlJsonObject: {} });
    const setFlowNodes = vi.fn();
    const { result } = renderHook(() => useFlowEditorNodeOperations({
      flowNodes: [],
      setFlowNodes,
      setFlowEdges: vi.fn(),
      setYamlJsonObject: store.getState().editPipelineYamlDocument,
      yamlJsonObjectRef: { current: {} },
      getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
      setCenter: vi.fn(),
      getZoom: () => 1,
      editorRef: { current: null },
      editorWidth: 800,
      editorHeight: 600,
    }));
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={result.current.onAddNode} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    await user.click(await screen.findByRole('menuitem', { name: label }));

    const node = { id, type, ...InitialNodeData[type] };
    expect(store.getState().yamlJsonObject).toEqual({ entry_point: id, nodes: [node] });
    expect(load(store.getState().yamlCode)).toEqual(store.getState().yamlJsonObject);
    expect(store.getState().stateKeyOrder).toEqual([]);
    expect(setFlowNodes).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument());
  });
});
