import { cleanup } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { renderExtensionCard } from '../../__tests__/graphExtensionTestUtils';
import { parallelYaml } from '../../__tests__/parallelTestUtils';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { ParallelNode } from './ParallelNode';

afterEach(cleanup);
describe('the fixed Parallel card', () => {
  it('renders all stored settings and exact result identities with editable controls, leaving YAML unchanged until an edit', async () => {
    const screen = renderExtensionCard(ParallelNode, parallelYaml);
    expect(await screen.findByLabelText('Branch 1 result key')).toHaveValue('stable_left');
    expect(screen.getByLabelText('Branch 2 Agent node')).toHaveValue('Agent_right');
    expect(screen.getByLabelText('Ordered list output')).toHaveValue('10');
    expect(screen.queryByText(/authoring is not enabled/u)).toBeNull();
    for (const field of screen.getAllByRole('textbox')) expect(field).toBeEnabled();
    expect(screen.getByLabelText('Maximum concurrent branches')).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Add fixed branch' })).toBeEnabled();
    expect(screen.container.querySelectorAll('.react-flow__handle.connectable').length).toBeGreaterThan(0);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(parallelYaml);
  });
  it('shows field-specific refusal for foreign Map configuration while retaining original data', async () => {
    const yaml = parallelYaml.replace('    wait: all', '    wait: any\n    reduction: ordered_collection\n    input_mapping: {task: {type: fixed, value: foreign}}');
    const screen = renderExtensionCard(ParallelNode, yaml);
    await screen.findByLabelText('Wait policy');
    expect(screen.getAllByText(/wait: Select all/u).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/reduction: This field/u).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/input_mapping: This field/u).length).toBeGreaterThan(0);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
  });
});
