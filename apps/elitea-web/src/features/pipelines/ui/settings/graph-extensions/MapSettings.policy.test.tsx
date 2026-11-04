import type { ReactNode } from 'react';
import { useContext } from 'react';
import userEvent from '@testing-library/user-event';
import { cleanup, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { currentExtensionDocument, renderExtensionCard } from '../../../__tests__/graphExtensionTestUtils';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { patchGraphExtensionNode } from '../../../lib/graphExtensions.helpers';
import { MapSettings } from './MapSettings';

// T-2A3 commit 8: Map On Error and Join (the shared design-system select),
// an LLM worker in the worker list, and a v1 document moved to v2 without
// changing its meaning.

const mapYaml = (policy: string): string => `entry_point: extension
state:
  items: {type: list, value: []}
  mapped: {type: list, value: []}
  keyed: {type: dict, value: {}}
  rendered: {type: str, value: ''}
nodes:
  - id: extension
    type: map
    worker: worker
    source: items
    item: entity
    index: item_index
    outputs: [rendered]
    destination: mapped
    max_items: 8
    max_concurrency: 2
${policy}    transition: END
  - id: worker
    type: state_modifier
    input: [entity, item_index]
    output: [rendered]
    template: '{{ entity }}'
  - id: drafter
    type: llm
    input: [entity]
    input_mapping: {task: {type: fixed, value: Draft}}
    output: [rendered]
`;

function EditableMapSettings(): ReactNode {
  const context = useContext(FlowEditorContext);
  const node = context?.yamlJsonObject.nodes?.find((entry) => entry.id === 'extension');
  if (!context || !node) return null;
  return <MapSettings node={node} disabled={Boolean(context.disabled)}
    change={(field, value) => context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, node.id, field, value))}
    remove={(field) => context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, node.id, field, undefined, true))} />;
}

const owner = () => currentExtensionDocument().nodes?.find((node) => node.id === 'extension');
const optionsOf = async (user: ReturnType<typeof userEvent.setup>, screen: ReturnType<typeof renderExtensionCard>, label: string) => {
  await user.click(screen.getByRole('combobox', { name: label }));
  const listbox = await screen.findByRole('listbox');
  const values = within(listbox).getAllByRole('option').map((option) => option.textContent);
  return { listbox, values };
};

afterEach(cleanup);

describe('Map On Error and Join', () => {
  it('offers merge only with stop and writes the exact YAML keys', async () => {
    const screen = renderExtensionCard(EditableMapSettings, mapYaml(''));
    const user = userEvent.setup();
    expect(await screen.findByRole('combobox', { name: 'On Error' })).toHaveTextContent('continue');
    let join = await optionsOf(user, screen, 'Join');
    expect(join.values).toEqual(['append', 'keyed']);
    await user.click(within(join.listbox).getByRole('option', { name: 'append' }));
    const onError = await optionsOf(user, screen, 'On Error');
    await user.click(within(onError.listbox).getByRole('option', { name: 'stop' }));
    expect(owner()).toMatchObject({ on_error: 'stop' });
    join = await optionsOf(user, screen, 'Join');
    expect(join.values).toEqual(['append', 'keyed', 'merge']);
    await user.click(within(join.listbox).getByRole('option', { name: 'merge' }));
    expect(owner()).toMatchObject({ on_error: 'stop', join: 'merge' });
    expect((await optionsOf(user, screen, 'On Error')).values).toEqual(['stop']);
  });

  it('moves a v1 document to v2 without changing its meaning', async () => {
    const screen = renderExtensionCard(EditableMapSettings, mapYaml('    reduction: ordered_collection\n'));
    const user = userEvent.setup();
    expect(await screen.findByLabelText('Reduction')).toHaveValue('ordered_collection');
    expect(screen.getByRole('combobox', { name: 'On Error' })).toHaveTextContent('stop');
    const join = await optionsOf(user, screen, 'Join');
    await user.click(within(join.listbox).getByRole('option', { name: 'keyed' }));
    expect(owner()).not.toHaveProperty('reduction');
    expect(owner()).toMatchObject({ on_error: 'stop', join: 'keyed' });
    expect(screen.queryByLabelText('Reduction')).toBeNull();
  });

  it('lists an LLM node as a worker', async () => {
    const screen = renderExtensionCard(EditableMapSettings, mapYaml(''));
    const worker = await screen.findByLabelText('Owned worker');
    expect([...(worker as HTMLSelectElement).options].map((option) => option.value)).toEqual(['', 'worker', 'drafter']);
  });
});
