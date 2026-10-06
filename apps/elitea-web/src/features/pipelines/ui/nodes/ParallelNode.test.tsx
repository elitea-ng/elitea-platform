import { cleanup } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { renderExtensionCard } from '../../__tests__/graphExtensionTestUtils';
import { parallelYaml } from '../../__tests__/parallelTestUtils';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { ParallelNode } from './ParallelNode';

afterEach(cleanup);
describe('stored fixed Parallel card with production authoring disabled', () => {
  it('renders all stored settings and exact result identities without enabling controls or changing YAML', async () => {
    const screen = renderExtensionCard(ParallelNode, parallelYaml);
    expect(await screen.findByLabelText('Branch 1 result key')).toHaveValue('stable_left');
    expect(screen.getByLabelText('Branch 2 Agent node')).toHaveValue('Agent_right');
    expect(screen.getByLabelText('Ordered list output')).toHaveValue('10');
    expect(screen.getByText(/authoring is not enabled/u)).toBeVisible();
    for (const field of screen.getAllByRole('textbox')) expect(field).toBeDisabled();
    for (const field of screen.getAllByRole('combobox')) {
      if (field.tagName === 'SELECT') expect(field).toBeDisabled();
      else expect(field).toHaveAttribute('aria-disabled', 'true');
    }
    expect(screen.getByLabelText('Maximum concurrent branches')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add fixed branch' })).toBeDisabled();
    expect(screen.container.querySelectorAll('.react-flow__handle.connectable').length).toBe(0);
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
