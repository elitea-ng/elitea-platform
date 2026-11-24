import userEvent from '@testing-library/user-event';
import { cleanup, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { aggregateNodeYaml, authoredExtensionYaml, currentExtensionDocument, renderExtensionCard } from '../../__tests__/graphExtensionTestUtils';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { parsePipelineYamlDocument } from '../../lib/pipelineYamlDocument.helpers';
import { AggregateNode } from './AggregateNode';

afterEach(cleanup);
const selectOption = async (user: ReturnType<typeof userEvent.setup>, screen: ReturnType<typeof renderExtensionCard>, label: string, optionName: string) => {
  await user.click(await screen.findByRole('combobox', { name: label }));
  const listbox = await screen.findByRole('listbox');
  await user.click(within(listbox).getByRole('option', { name: optionName }));
};
const optionLabelsOf = async (user: ReturnType<typeof userEvent.setup>, screen: ReturnType<typeof renderExtensionCard>, label: string) => {
  await user.click(await screen.findByRole('combobox', { name: label }));
  const listbox = await screen.findByRole('listbox');
  const values = within(listbox).getAllByRole('option').map((option) => option.textContent);
  await user.keyboard('{Escape}');
  return values;
};
describe('AggregateNode authored YAML', () => {
  it('renders a count operation and unknown list elements without inserting optional fields', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml);
    const screen = renderExtensionCard(AggregateNode, yaml);
    expect(await screen.findByRole('combobox', { name: 'Operation 1' })).toHaveTextContent('count_rows');
    expect(screen.queryByLabelText('Value field pointer 1')).toBeNull();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')).not.toHaveProperty('layout');
  });
  it('authors an explicit integer reduction and field policy in saved YAML without inferring a schema', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const before = currentExtensionDocument().state;
    const user = userEvent.setup();
    await selectOption(user, screen, 'Operation 1', 'sum_int');
    await user.type(screen.getByLabelText('Value field pointer 1'), '/cost');
    await selectOption(user, screen, 'Missing value 1', 'skip');
    await selectOption(user, screen, 'Null value 1', 'error');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['operations']).toEqual([
      { operation: 'sum_int', output: 'count', field: { path: '/cost', missing: 'skip', null: 'error' } },
    ]);
    expect(currentExtensionDocument().state).toEqual(before);
    expect(usePipelineYamlStore.getState().stateKeyOrder.slice(0, 2)).toEqual(['10', '2']);
  });
  it('authors grouping independently from operation outputs and reports equal names across result sections', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add group field' }));
    await user.type(screen.getByLabelText('Group field pointer 1'), '/country');
    await user.type(screen.getByLabelText('Group key name 1'), 'count');
    await selectOption(user, screen, 'Missing group field 1', 'null');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['group_by']).toEqual([
      { path: '/country', output: 'count', missing: 'null' },
    ]);
    expect(screen.getByText(/already appears in group_by/u)).toBeTruthy();
  });
  it.each(['sum_int', 'min_int', 'max_int'])('defaults null: error and hides keep when switching to %s', async (operation) => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    await selectOption(user, screen, 'Operation 1', operation);
    const nullPolicy = screen.getByRole('combobox', { name: 'Null value 1' });
    expect(nullPolicy).toHaveTextContent('error');
    expect(await optionLabelsOf(user, screen, 'Null value 1')).toEqual(['error', 'skip']);
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['operations']).toEqual([
      { operation, output: 'count', field: { path: '', missing: 'error', null: 'error' } },
    ]);
    await selectOption(user, screen, 'Operation 1', 'first');
    expect(screen.getByRole('combobox', { name: 'Null value 1' })).toHaveTextContent('keep');
  });
  it('normalizes a stored keep policy to error when switching a collect to sum_int', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml.replace('{operation: count_rows, output: count}',
      '{operation: collect, output: count, field: {path: /n, null: keep}}'));
    const screen = renderExtensionCard(AggregateNode, yaml);
    await selectOption(userEvent.setup(), screen, 'Operation 1', 'sum_int');
    expect(screen.getByRole('combobox', { name: 'Null value 1' })).toHaveTextContent('error');
  });
  it('offers regroup and collect_rows retention without none, with help text', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Advanced' }));
    const regroup = await screen.findByRole('combobox', { name: 'Regroup' });
    expect(regroup).toHaveTextContent('none');
    await selectOption(user, screen, 'Regroup', 'parent');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['regroup']).toBe('parent');
    expect(screen.getByText(/Parent rebuilds each split parent row/u)).toBeTruthy();
    expect(screen.getByText(/Empty input gives one row/u)).toBeTruthy();
    expect(screen.getByText(/Each result row holds the group fields and the operation results side by side/u)).toBeTruthy();
    expect(screen.queryByText(/keeps group keys and operation values separate/u)).toBeNull();
    await selectOption(user, screen, 'Operation 1', 'collect_rows');
    expect(await optionLabelsOf(user, screen, 'Retain fields')).toEqual(['all', 'only', 'except']);
  });
  it('offers groups limit for aggregate', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Advanced' }));
    await user.click(await screen.findByText('Processing limits'));
    expect(screen.getByLabelText('groups')).toBeTruthy();
  });
  it('removes irrelevant fields only on an explicit operation change', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml.replace('{operation: count_rows, output: count}',
      '{operation: collect, output: count, field: {path: /tags}, merge_lists: true}'));
    const screen = renderExtensionCard(AggregateNode, yaml);
    const user = userEvent.setup();
    await selectOption(user, screen, 'Operation 1', 'count_rows');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['operations']).toEqual([
      { operation: 'count_rows', output: 'count' },
    ]);
  });
  it('uses an explicit SplitOut input layout and collect_rows retention controls', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    await selectOption(user, screen, 'Input row layout', 'split_out');
    await selectOption(user, screen, 'Operation 1', 'collect_rows');
    expect(screen.getByRole('combobox', { name: 'Retain fields' })).toHaveTextContent('all');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['layout']).toBe('split_out');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['operations']).toEqual([
      { operation: 'collect_rows', output: 'count', retain: { mode: 'all' } },
    ]);
  });
  it('preserves unrelated malformed rows and unknown operation metadata during a targeted edit', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml.replace('[{operation: count_rows, output: count}]',
      '[{operation: count_rows, output: count, vendor: {opaque: null}}, null]'));
    const screen = renderExtensionCard(AggregateNode, yaml);
    const user = userEvent.setup();
    const field = await screen.findByLabelText('Result field name 1');
    await user.clear(field); await user.type(field, 'total');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['operations']).toEqual([
      { operation: 'count_rows', output: 'total', vendor: { opaque: null } }, null,
    ]);
  });
  it('preserves opaque collect_rows retention metadata on an explicit mode change', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml.replace('[{operation: count_rows, output: count}]',
      '[{operation: collect_rows, output: count, retain: {mode: only, fields: [{path: /name, output: name}], vendor: null, opaque: {keep: [null, {fields: legacy}]}}, vendor: null}, null]'));
    const screen = renderExtensionCard(AggregateNode, yaml);
    const before = currentExtensionDocument();
    const originalOrder = [...usePipelineYamlStore.getState().stateKeyOrder];
    const user = userEvent.setup();
    await selectOption(user, screen, 'Retain fields', 'all');
    const operations = [
      { operation: 'collect_rows', output: 'count', retain: { mode: 'all', vendor: null, opaque: { keep: [null, { fields: 'legacy' }] } }, vendor: null },
      null,
    ];
    const expected = { ...before, nodes: before.nodes?.map((entry) => entry.id === 'extension' ? { ...entry, operations } : entry) };
    expect(currentExtensionDocument()).toEqual(expected);
    expect(parsePipelineYamlDocument(usePipelineYamlStore.getState().yamlCode).yamlJsonObject).toEqual(expected);
    expect(usePipelineYamlStore.getState().stateKeyOrder).toEqual(originalOrder);
  });
  it('disables operation, grouping and layout controls while editing is disabled', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml), true);
    expect(await screen.findByRole('combobox', { name: 'Operation 1' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('combobox', { name: 'Input row layout' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Add group field' })).toBeDisabled();
  });
});
