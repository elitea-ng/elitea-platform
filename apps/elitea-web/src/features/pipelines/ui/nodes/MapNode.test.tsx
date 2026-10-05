import userEvent from '@testing-library/user-event';
import { cleanup } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { authoredExtensionYaml, currentExtensionDocument, mapNodeYaml, renderExtensionCard } from '../../__tests__/graphExtensionTestUtils';
import { readPipelineStateOrder } from '../../lib/pipelineYamlState.helpers';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { MapNode } from './MapNode';

afterEach(cleanup);
describe('MapNode authored YAML', () => {
  it('renders explicit worker, local bindings, ordered reduction and bounds without changing original bytes', async () => {
    const yaml = authoredExtensionYaml(mapNodeYaml);
    const screen = renderExtensionCard(MapNode, yaml);
    await screen.findByLabelText('Owned worker');
    expect(screen.getByLabelText('Child item channel')).toHaveValue('entity');
    expect(screen.getByLabelText('Child index channel')).toHaveValue('item_index');
    expect(screen.getByLabelText('Reduction')).toHaveValue('ordered_collection');
    expect(screen.getByLabelText('Maximum concurrent items')).toHaveValue(2);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
    expect(currentExtensionDocument().state).not.toHaveProperty('entity');
  });
  it('edits a fanout bound and preserves opaque state, state order and child ownership in saved YAML', async () => {
    const screen = renderExtensionCard(MapNode, authoredExtensionYaml(mapNodeYaml));
    const before = currentExtensionDocument();
    const user = userEvent.setup();
    const field = await screen.findByLabelText('Maximum concurrent items');
    await user.clear(field); await user.type(field, '4');
    const saved = usePipelineYamlStore.getState();
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['max_concurrency']).toBe(4);
    expect(currentExtensionDocument().state).toEqual(before.state);
    expect(readPipelineStateOrder(saved.yamlCode)).toEqual(['10', '2', 'source_rows', 'source_object', 'item_result', 'prefix']);
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'render_item')).toEqual(before.nodes?.find((node) => node.id === 'render_item'));
  });
  it('changes only the exact owned worker input and creates no global descriptor', async () => {
    const screen = renderExtensionCard(MapNode, authoredExtensionYaml(mapNodeYaml));
    const user = userEvent.setup();
    await user.click(await screen.findByLabelText('entity'));
    const document = currentExtensionDocument();
    expect(document.nodes?.find((node) => node.id === 'render_item')?.input).toEqual(['item_index', 'prefix']);
    expect(document.nodes?.find((node) => node.id === 'extension')?.['item']).toBe('entity');
    expect(document.state).not.toHaveProperty('entity');
  });
  it('withholds local controls when two Maps try to own the same worker', async () => {
    const yaml = authoredExtensionYaml(mapNodeYaml) + '  - {id: other_map, type: map, worker: render_item}\n';
    const screen = renderExtensionCard(MapNode, yaml);
    await screen.findByLabelText('Owned worker');
    expect(screen.queryByLabelText('entity')).toBeNull();
    expect(screen.getAllByText(/exactly one Map/u).length).toBeGreaterThan(0);
  });
  it('removes the selected worker transition only after an explicit action', async () => {
    const yaml = authoredExtensionYaml(mapNodeYaml.replace('    template:', '    transition: END\n    template:'));
    const screen = renderExtensionCard(MapNode, yaml);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Remove owned worker transition' }));
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'render_item')).not.toHaveProperty('transition');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.transition).toBe('END');
  });
  it('keeps an unknown authored reduction visible and preserves malformed sibling output entries', async () => {
    const yaml = authoredExtensionYaml(mapNodeYaml.replace('reduction: ordered_collection', 'reduction: authored_unknown')
      .replace('outputs: [item_result]', 'outputs: [item_result, null]'));
    const screen = renderExtensionCard(MapNode, yaml);
    const user = userEvent.setup();
    expect(await screen.findByLabelText('Reduction')).toHaveValue('authored_unknown');
    await user.selectOptions(screen.getByLabelText('Worker output 1'), 'item_result');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['outputs']).toEqual(['item_result', null]);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
  });
  it('disables every mapping field and worker action when editing is disabled', async () => {
    const screen = renderExtensionCard(MapNode, authoredExtensionYaml(mapNodeYaml), true);
    expect(await screen.findByLabelText('Child item channel')).toBeDisabled();
    expect(screen.getByLabelText('Owned worker')).toBeDisabled();
    expect(screen.getByLabelText('entity')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add worker output' })).toBeDisabled();
  });
});
