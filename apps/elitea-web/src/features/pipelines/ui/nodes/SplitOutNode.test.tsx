import userEvent from '@testing-library/user-event';
import { cleanup } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { authoredExtensionYaml, currentExtensionDocument, renderExtensionCard, splitNodeYaml } from '../../__tests__/graphExtensionTestUtils';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { parsePipelineYamlDocument } from '../../lib/pipelineYamlDocument.helpers';
import { SplitOutNode } from './SplitOutNode';

afterEach(cleanup);
describe('SplitOutNode authored YAML', () => {
  it('reads an escaped pointer and declared object without adding defaults to authored YAML', async () => {
    const yaml = authoredExtensionYaml(splitNodeYaml);
    const screen = renderExtensionCard(SplitOutNode, yaml);
    expect(await screen.findByLabelText('List field pointer')).toHaveValue('/a~1b/orders');
    expect(screen.getByLabelText('Source state variable')).toHaveValue('source_object');
    expect(screen.getByText('Authored default at this pointer: [null,{"x":2}]')).toBeInTheDocument();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
    expect(currentExtensionDocument().nodes?.find((node) => node.id === 'extension')).not.toHaveProperty('retain');
  });
  it('changes missing-list behavior and retains state, unknown defaults and exactly the authored nodes', async () => {
    const screen = renderExtensionCard(SplitOutNode, authoredExtensionYaml(splitNodeYaml));
    const before = currentExtensionDocument();
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Missing list'), 'empty');
    const document = currentExtensionDocument();
    expect(document.state).toEqual(before.state);
    expect(document.nodes?.map((node) => node.id)).toEqual(['start', 'extension']);
    expect(document.nodes?.find((node) => node.id === 'extension')?.['missing_list']).toBe('empty');
    expect(usePipelineYamlStore.getState().stateKeyOrder.slice(0, 2)).toEqual(['10', '2']);
  });
  it('keeps source state unchanged when the user changes the source location mode', async () => {
    const screen = renderExtensionCard(SplitOutNode, authoredExtensionYaml(splitNodeYaml));
    const before = currentExtensionDocument().state;
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('List location'), 'list');
    const node = currentExtensionDocument().nodes?.find((entry) => entry.id === 'extension');
    expect(node?.['split']).toEqual({ mode: 'list' });
    expect(node?.['source']).toBe('source_object');
    expect(currentExtensionDocument().state).toEqual(before);
    expect(screen.queryByLabelText('List field pointer')).toBeNull();
    expect(screen.getAllByText(/must declare list/u).length).toBeGreaterThan(0);
  });
  it('authors retention pointers, literal aliases and explicit missing behavior', async () => {
    const screen = renderExtensionCard(SplitOutNode, authoredExtensionYaml(splitNodeYaml));
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Retain fields'), 'only');
    await user.click(screen.getByRole('button', { name: 'Add retained field' }));
    await user.type(screen.getByLabelText('Retained field pointer 1'), '/a~1b');
    await user.type(screen.getByLabelText('Retained field name 1'), 'customer.name');
    await user.selectOptions(screen.getByLabelText('Missing retained field 1'), 'null');
    expect(currentExtensionDocument().nodes?.find((entry) => entry.id === 'extension')?.['retain']).toEqual({
      mode: 'only', fields: [{ path: '/a~1b', output: 'customer.name', missing: 'null' }],
    });
  });
  it('offers only valid retention for direct lists without inserting optional defaults', async () => {
    const yaml = authoredExtensionYaml(splitNodeYaml.replace('source: source_object', 'source: source_rows')
      .replace('split: {mode: row_field, path: /a~1b/orders}', 'split: {mode: list}'));
    const screen = renderExtensionCard(SplitOutNode, yaml);
    const retention = await screen.findByLabelText('Retain fields');
    expect(retention.querySelectorAll('option')).toHaveLength(1);
    expect(retention).toHaveValue('none');
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
  });
  it.each(['/a~1b/orders', null])('preserves opaque split fields and compatible path %s across mode changes', async (path) => {
    const yaml = authoredExtensionYaml(splitNodeYaml.replace(
      'split: {mode: row_field, path: /a~1b/orders}',
      `split: {mode: row_field, path: ${path ?? 'null'}, vendor: null, opaque: {keep: [null, {mode: legacy}]}}`,
    )).replace('type: printer', 'type: printer\n    vendor: {keep: null}');
    const screen = renderExtensionCard(SplitOutNode, yaml);
    const before = currentExtensionDocument();
    const originalOrder = [...usePipelineYamlStore.getState().stateKeyOrder];
    const originalSplit = before.nodes?.find((entry) => entry.id === 'extension')?.['split'] as Record<string, unknown>;
    const user = userEvent.setup();
    const assertSavedSplit = (split: Record<string, unknown>) => {
      const expected = { ...before, nodes: before.nodes?.map((entry) => entry.id === 'extension' ? { ...entry, split } : entry) };
      expect(currentExtensionDocument()).toEqual(expected);
      expect(parsePipelineYamlDocument(usePipelineYamlStore.getState().yamlCode).yamlJsonObject).toEqual(expected);
      expect(usePipelineYamlStore.getState().stateKeyOrder).toEqual(originalOrder);
    };
    await user.selectOptions(await screen.findByLabelText('List location'), 'rows_field');
    assertSavedSplit({ ...originalSplit, mode: 'rows_field' });
    await user.selectOptions(screen.getByLabelText('List location'), 'list');
    const { path: _removedPath, ...withoutPath } = originalSplit;
    assertSavedSplit({ ...withoutPath, mode: 'list' });
    await user.selectOptions(screen.getByLabelText('List location'), 'row_field');
    assertSavedSplit({ ...withoutPath, mode: 'row_field', path: '' });
  });
  it.each([
    ['only', 'except', [{ path: '/vendor', output: 'kept', missing: 'null' }], []],
    ['except', 'only', ['vendor', 'literal.name'], []],
    ['only', 'none', [{ path: '/vendor', output: 'kept' }], undefined],
    ['except', 'all', ['vendor'], undefined],
  ])('preserves foreign retention fields when %s changes to %s', async (mode, next, fields, expectedFields) => {
    const retain = { mode, fields, vendor: null, opaque: { keep: [null, { fields: 'legacy' }] } };
    const yaml = authoredExtensionYaml(splitNodeYaml.replace('    destination: item', `    retain: ${JSON.stringify(retain)}\n    destination: item`));
    const screen = renderExtensionCard(SplitOutNode, yaml);
    const before = currentExtensionDocument();
    const originalOrder = [...usePipelineYamlStore.getState().stateKeyOrder];
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Retain fields'), next);
    const expectedRetain = { mode: next, vendor: null, opaque: retain.opaque, ...(expectedFields === undefined ? {} : { fields: expectedFields }) };
    const expected = { ...before, nodes: before.nodes?.map((entry) => entry.id === 'extension' ? { ...entry, retain: expectedRetain } : entry) };
    expect(currentExtensionDocument()).toEqual(expected);
    expect(parsePipelineYamlDocument(usePipelineYamlStore.getState().yamlCode).yamlJsonObject).toEqual(expected);
    expect(usePipelineYamlStore.getState().stateKeyOrder).toEqual(originalOrder);
  });
  it('disables pointer, retention and source controls while the card is disabled', async () => {
    const screen = renderExtensionCard(SplitOutNode, authoredExtensionYaml(splitNodeYaml), true);
    expect(await screen.findByLabelText('List field pointer')).toBeDisabled();
    expect(screen.getByLabelText('Retain fields')).toBeDisabled();
    expect(screen.getByLabelText('Source state variable')).toBeDisabled();
  });
});
