import { describe, expect, it } from 'vitest';
import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { graphShapingIssues } from './graphShapingAdmission.helpers';
import { collectGraphAdmissionIssues } from './graphAdmission.helpers';

const state = { records: 'list', expanded: { type: 'list', value: [] }, row: 'dict', grouped: 'list', text: 'str' };
const split: YamlPipelineNode = { id: 'split', type: 'split_out', source: 'records', split: { mode: 'list' }, destination: 'item', output: ['expanded'], transition: 'END' };
const aggregate: YamlPipelineNode = { id: 'aggregate', type: 'aggregate', source: 'expanded', operations: [{ operation: 'count_rows', output: 'count' }], output: ['grouped'] };
const documentFor = (value: unknown): YamlPipelineDocument => { const node = value as YamlPipelineNode; return { entry_point: node.id, state, nodes: [node] }; };
const fields = (node: unknown): readonly string[] => graphShapingIssues(documentFor(node)).map((issue) => issue.field);

describe('explicit data shaping contracts', () => {
  it('accepts coarse list/object declarations without element or field schemas', () => {
    expect(graphShapingIssues(documentFor(split))).toEqual([]);
    expect(graphShapingIssues(documentFor(aggregate))).toEqual([]);
    expect(graphShapingIssues({ ...documentFor(split), state: { ...state, records: { type: 'list', value: [null, 1, { arbitrary: true }] } } })).toEqual([]);
  });
  it('retains production type refusal while reporting valid private shaping contracts', () => {
    expect(collectGraphAdmissionIssues(documentFor(split)).filter((issue) => issue.rule === 'node.type')).toHaveLength(1);
    expect(collectGraphAdmissionIssues(documentFor(split)).filter((issue) => issue.rule === 'node.extension-configuration')).toEqual([]);
  });
  it.each([
    [{ source: 'text' }, 'source'], [{ source: 'missing' }, 'source'], [{ source: 'messages' }, 'source'],
    [{ output: [] }, 'output'], [{ output: ['records'] }, 'output[0]'], [{ output: ['text'] }, 'output[0]'],
    [{ output: ['expanded', 'grouped'] }, 'output'], [{ output: [null] }, 'output[0]'],
    [{ destination: '' }, 'destination'], [{ destination: '\u0085' }, 'destination'],
    [{ destination: '界'.repeat(86) }, 'destination'], [{ guessed_schema: {} }, 'guessed_schema'],
    [{ transition: 2 }, 'transition'], [{ split: { mode: 'list', path: '/items' } }, 'split.path'],
    [{ retain: { mode: 'all' } }, 'retain.mode'], [{ retain: null }, 'retain'],
    [{ split: { mode: 'rows_field', path: '/bad~2' } }, 'split.path'],
    [{ limits: { input_items: 10_001 } }, 'limits.input_items'], [{ limits: { bytes: 0 } }, 'limits.bytes'],
    [{ limits: { unknown: 2 } }, 'limits.unknown'], [{ missing_list: 'skip' }, 'missing_list'],
    [{ null_list: null }, 'null_list'], [{ remove_source: 'true' }, 'remove_source'],
  ])('refuses exact SplitOut field %s at %s', (patch, field) => {
    expect(fields({ ...split, ...patch })).toContain(field);
  });
  it('accepts escaped/control pointer tokens and partial lower limits for object sources', () => {
    expect(fields({ ...split, source: 'row', split: { mode: 'row_field', path: '/a~1b/\u0000' },
      limits: { bytes: 262144 }, missing_list: 'empty', null_list: 'empty', remove_source: false, transition: null })).toEqual([]);
  });
  it('refuses duplicate retained aliases and predictable item-name collisions', () => {
    const configured = { ...split, source: 'row', split: { mode: 'row_field', path: '/items' }, retain: {
      mode: 'only', fields: [{ path: '/a', output: 'item' }, { path: '/b', output: 'item' }],
    } };
    expect(fields(configured)).toContain('retain.fields[1]');
    expect(fields(configured)).toContain('destination');
  });
  it.each([
    [{ operations: [] }, 'operations'], [{ operations: Array.from({ length: 65 }, () => ({ operation: 'count_rows', output: 'x' })) }, 'operations'],
    [{ layout: 'auto' }, 'layout'], [{ operations: [{ operation: 'sum', output: 'total' }] }, 'operations[0].operation'],
    [{ operations: [{ operation: 'sum_int', output: 'total', field: { path: '' } }] }, 'operations[0].field.path'],
    [{ operations: [{ operation: 'count_rows', output: 'count', field: { path: '/cost' } }] }, 'operations[0].field'],
    [{ operations: [{ operation: 'first', output: 'first', field: { path: '/x' }, merge_lists: true }] }, 'operations[0].merge_lists'],
    [{ group_by: [{ path: '/region', output: 'region', missing: 'skip' }] }, 'group_by[0].missing'],
    [{ operations: [{ operation: 'collect', output: 'items', field: { path: '/x', null: 'null' } }] }, 'operations[0].field.null'],
  ])('refuses exact Aggregate field %s at %s', (patch, field) => {
    expect(fields({ ...aggregate, ...patch })).toContain(field);
  });
  it('checks duplicates within sections and permits equal group and operation aliases', () => {
    const configured = { ...aggregate, group_by: [{ path: '/a', output: 'count' }] };
    expect(fields(configured)).toEqual([]);
    expect(fields({ ...configured, group_by: [{ path: '/a', output: 'a' }, { path: '/b', output: 'a' }] })).toContain('group_by[1]');
    expect(fields({ ...configured, operations: [{ operation: 'count_rows', output: 'count' }, { operation: 'count_rows', output: 'count' }] })).toContain('operations[1]');
  });
  it.each(['collect', 'sum_int', 'min_int', 'max_int', 'first', 'last'])('accepts explicit %s selectors with unknown values', (operation) => {
    expect(fields({ ...aggregate, operations: [{ operation, output: 'value', field: { path: '/opaque', missing: 'skip', null: 'error' } }] })).toEqual([]);
  });
});
