import userEvent from '@testing-library/user-event';
import { cleanup } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { aggregateNodeYaml, authoredExtensionYaml, currentExtensionDocument, renderExtensionCard } from '../../__tests__/graphExtensionTestUtils';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { parsePipelineYamlDocument } from '../../lib/pipelineYamlDocument.helpers';
import { AggregateNode } from './AggregateNode';

afterEach(cleanup);
describe('AggregateNode authored YAML', () => {
  it('renders a count operation and unknown list elements without inserting optional fields', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml);
    const screen = renderExtensionCard(AggregateNode, yaml);
    expect(await screen.findByLabelText('Operation 1')).toHaveValue('count_rows');
    expect(screen.queryByLabelText('Value field pointer 1')).toBeNull();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')).not.toHaveProperty('layout');
  });
  it('authors an explicit integer reduction and field policy in saved YAML without inferring a schema', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const before = currentExtensionDocument().state;
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Operation 1'), 'sum_int');
    await user.type(screen.getByLabelText('Value field pointer 1'), '/cost');
    await user.selectOptions(screen.getByLabelText('Missing value 1'), 'skip');
    await user.selectOptions(screen.getByLabelText('Null value 1'), 'error');
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
    await user.selectOptions(screen.getByLabelText('Missing group field 1'), 'null');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['group_by']).toEqual([
      { path: '/country', output: 'count', missing: 'null' },
    ]);
    expect(screen.getByText(/already appears in group_by/u)).toBeTruthy();
  });
  it.each(['sum_int', 'min_int', 'max_int'])('defaults null: error and hides keep when switching to %s', async (operation) => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Operation 1'), operation);
    const nullPolicy = screen.getByLabelText('Null value 1');
    expect(nullPolicy).toHaveValue('error');
    expect([...nullPolicy.querySelectorAll('option')].map((option) => option.value)).toEqual(['error', 'skip']);
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['operations']).toEqual([
      { operation, output: 'count', field: { path: '', missing: 'error', null: 'error' } },
    ]);
    await user.selectOptions(screen.getByLabelText('Operation 1'), 'first');
    expect(screen.getByLabelText('Null value 1')).toHaveValue('keep');
  });
  it('normalizes a stored keep policy to error when switching a collect to sum_int', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml.replace('{operation: count_rows, output: count}',
      '{operation: collect, output: count, field: {path: /n, null: keep}}'));
    const screen = renderExtensionCard(AggregateNode, yaml);
    await userEvent.setup().selectOptions(await screen.findByLabelText('Operation 1'), 'sum_int');
    expect(screen.getByLabelText('Null value 1')).toHaveValue('error');
  });
  it('offers regroup and collect_rows retention without none, with help text', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    const regroup = await screen.findByLabelText('Regroup');
    expect(regroup).toHaveValue('none');
    await user.selectOptions(regroup, 'parent');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['regroup']).toBe('parent');
    expect(screen.getByText(/Parent rebuilds each split parent row/u)).toBeTruthy();
    expect(screen.getByText(/Empty input gives one row/u)).toBeTruthy();
    await user.selectOptions(screen.getByLabelText('Operation 1'), 'collect_rows');
    const retain = screen.getByLabelText('Retain fields');
    expect([...retain.querySelectorAll('option')].map((option) => option.value)).toEqual(['all', 'only', 'except']);
  });
  it('offers groups limit for aggregate', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    await userEvent.setup().click(await screen.findByText('Processing limits'));
    expect(screen.getByLabelText('groups')).toBeTruthy();
  });
  it('removes irrelevant fields only on an explicit operation change', async () => {
    const yaml = authoredExtensionYaml(aggregateNodeYaml.replace('{operation: count_rows, output: count}',
      '{operation: collect, output: count, field: {path: /tags}, merge_lists: true}'));
    const screen = renderExtensionCard(AggregateNode, yaml);
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Operation 1'), 'count_rows');
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')?.['operations']).toEqual([
      { operation: 'count_rows', output: 'count' },
    ]);
  });
  it('uses an explicit SplitOut input layout and collect_rows retention controls', async () => {
    const screen = renderExtensionCard(AggregateNode, authoredExtensionYaml(aggregateNodeYaml));
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Input row layout'), 'split_out');
    await user.selectOptions(screen.getByLabelText('Operation 1'), 'collect_rows');
    expect(screen.getByLabelText('Retain fields')).toHaveValue('all');
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
    await user.selectOptions(await screen.findByLabelText('Retain fields'), 'all');
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
    expect(await screen.findByLabelText('Operation 1')).toBeDisabled();
    expect(screen.getByLabelText('Input row layout')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add group field' })).toBeDisabled();
  });
});
