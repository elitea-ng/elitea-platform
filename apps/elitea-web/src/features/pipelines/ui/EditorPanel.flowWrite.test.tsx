import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { load } from 'js-yaml';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { PIPELINE_165_YAML } from '../__tests__/pipeline165Fixture';
import { renderHookWithProviders, renderWithRouterAndProject } from '../__tests__/testUtils';
import { usePipelineEditorStore } from '../model/pipelineEditorStore';
import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { EditorPanel } from './EditorPanel';
import { usePipelineVersionSync } from './usePipelineEditorLifecycle';

/*
 * No `vi.mock`: this renders the REAL EditorPanel -> lazily loaded FlowWrapper/FlowEditor -> real Add node menu,
 * exactly like `pipelineNumericNodeIds.integration.test.tsx`. That is the only way to reach EditorPanel's own
 * `setYamlJsonObject`, which it hands to the flow pane.
 */
installCodeMirrorTestPolyfills();

type Doc = { nodes: Record<string, unknown>[] };

/** Seeds the canvas from the saved version, then (optionally) swaps the stored document for one the strict YAML serializer refuses. */
function seed(options: { readonly refusable: boolean }): void {
  renderHookWithProviders(() =>
    usePipelineVersionSync({
      isCreateMode: false,
      versionDetails: { id: '7', application_id: '3', name: 'v1', status: 'draft', instructions: PIPELINE_165_YAML },
      versionId: 7,
    }),
  );
  if (!options.refusable) return;
  // Same member the "keeps the stored YAML and shows a readable error…" test in EditorPanel.test.tsx uses.
  const document = load(PIPELINE_165_YAML) as Doc;
  document.nodes[1] = { ...document.nodes[1], input_mapping: { folder: { type: 'fixed', value: null, enum: undefined } } };
  usePipelineYamlStore.setState({ yamlJsonObject: document });
}

async function addAgentNode(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByRole('button', { name: 'Add node' }));
  await user.click(await screen.findByRole('menuitem', { name: 'Agent' }));
}

const canvasNodeIds = (container: HTMLElement): string[] =>
  Array.from(container.querySelectorAll('.react-flow__node')).map((element) => element.getAttribute('data-id') ?? '');

beforeEach(() => {
  usePipelineYamlStore.setState({ yamlCode: '', yamlJsonObject: {}, initYamlCode: '', initYamlJsonObject: {}, resetFlag: false, layoutVersion: undefined });
  usePipelineEditorStore.setState({ nodes: [], edges: [], stateValidationErrors: {} });
});

describe('EditorPanel: flow-pane document writes', () => {
  it('adds the node to the canvas when the document write is stored (control)', async () => {
    seed({ refusable: false });
    const { container } = renderWithRouterAndProject(<EditorPanel setYamlDirty={vi.fn()} stopRun={vi.fn()} />, '1');
    const user = userEvent.setup();
    await waitFor(() => expect(canvasNodeIds(container).length).toBeGreaterThan(0), { timeout: 20000 });
    expect(screen.queryByText('Failed to load the flow editor')).not.toBeInTheDocument();
    const before = canvasNodeIds(container);

    await addAgentNode(user);

    await waitFor(() => expect(canvasNodeIds(container)).toContain('Agent_1'));
    expect(canvasNodeIds(container)).toHaveLength(before.length + 1);
    expect((load(usePipelineYamlStore.getState().yamlCode) as Doc).nodes.map((node) => node['id'])).toContain('Agent_1');
  }, 30000);

  it('adds no canvas node, and keeps the stored YAML, when the serializer refuses the document', async () => {
    seed({ refusable: true });
    const storedYaml = usePipelineYamlStore.getState().yamlCode;
    const storedDocument = usePipelineYamlStore.getState().yamlJsonObject;
    const { container } = renderWithRouterAndProject(<EditorPanel setYamlDirty={vi.fn()} stopRun={vi.fn()} />, '1');
    const user = userEvent.setup();
    await waitFor(() => expect(canvasNodeIds(container).length).toBeGreaterThan(0), { timeout: 20000 });
    expect(screen.queryByText('Failed to load the flow editor')).not.toBeInTheDocument();
    const before = canvasNodeIds(container);

    await addAgentNode(user);
    // The refusal is synchronous inside the click handler; let any deferred canvas update (it would be a setFlowNodes + reveal timer) settle before asserting absence.
    await new Promise((resolve) => setTimeout(resolve, 300));

    expect(canvasNodeIds(container)).toEqual(before);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(storedYaml);
    expect(usePipelineYamlStore.getState().yamlJsonObject).toBe(storedDocument);
  }, 30000);
});
