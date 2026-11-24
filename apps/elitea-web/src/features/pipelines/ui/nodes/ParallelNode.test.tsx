import userEvent from '@testing-library/user-event';
import { cleanup } from '@testing-library/react';
import { ReactFlowProvider } from '@xyflow/react';
import { afterEach, describe, expect, it } from 'vitest';
import { renderExtensionCard } from '../../__tests__/graphExtensionTestUtils';
import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { parsePipelineYamlDocument } from '../../lib/pipelineYamlDocument.helpers';
import { parallelYaml } from '../../__tests__/parallelTestUtils';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { ParallelNode } from './ParallelNode';

afterEach(cleanup);
describe('the fixed Parallel card', () => {
  it('renders all stored settings and exact result identities with editable controls, leaving YAML unchanged until an edit', async () => {
    const screen = renderExtensionCard(ParallelNode, parallelYaml);
    expect(await screen.findByLabelText('Branch 1 result key')).toHaveValue('stable_left');
    expect(screen.getByRole('combobox', { name: 'Branch 2 Agent, LLM or Code node' })).toHaveTextContent('Agent_right');
    expect(screen.getByRole('combobox', { name: 'Ordered list output' })).toHaveTextContent('10');
    expect(screen.queryByText(/authoring is not enabled/u)).toBeNull();
    for (const field of screen.getAllByRole('textbox')) expect(field).toBeEnabled();
    expect(screen.getByLabelText('Maximum concurrent branches')).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Add fixed branch' })).toBeEnabled();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(parallelYaml);
  });
  it('shows field-specific refusal for foreign Map configuration while retaining original data', async () => {
    const yaml = parallelYaml.replace('    wait: all', '    wait: any\n    reduction: ordered_collection\n    input_mapping: {task: {type: fixed, value: foreign}}');
    const screen = renderExtensionCard(ParallelNode, yaml);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Advanced' }));
    await screen.findByRole('combobox', { name: 'Wait policy' });
    expect(screen.getAllByText(/wait: Select all/u).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/reduction: This field/u).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/input_mapping: This field/u).length).toBeGreaterThan(0);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
  });
  it('renders connectable target and source handles on the canvas view-only card', async () => {
    const parsed = parsePipelineYamlDocument(parallelYaml);
    const ctx = buildFlowEditorContextValue({ yamlJsonObject: parsed.yamlJsonObject });
    const props = { id: 'extension', type: 'parallel', data: {}, selected: false } as unknown as Parameters<typeof ParallelNode>[0];
    const screen = renderWithRouterAndProject(
      <ReactFlowProvider>
        <FlowEditorContext.Provider value={ctx}>
          <ParallelNode {...props} />
        </FlowEditorContext.Provider>
      </ReactFlowProvider>,
      undefined,
    );
    await screen.findByTestId('pipeline-node-v2-card-extension');
    expect(screen.container.querySelectorAll('.react-flow__handle.connectable').length).toBeGreaterThan(0);
  });
});
