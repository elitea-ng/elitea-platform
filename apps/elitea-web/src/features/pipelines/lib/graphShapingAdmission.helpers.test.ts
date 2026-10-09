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
  it('checks duplicates within sections and refuses equal group and operation aliases', () => {
    const configured = { ...aggregate, group_by: [{ path: '/a', output: 'count' }] };
    expect(fields(configured)).toContain('operations[0]');
    expect(fields({ ...configured, group_by: [{ path: '/a', output: 'a' }] })).toEqual([]);
    expect(fields({ ...configured, group_by: [{ path: '/a', output: 'a' }, { path: '/b', output: 'a' }] })).toContain('group_by[1]');
    expect(fields({ ...configured, operations: [{ operation: 'count_rows', output: 'count' }, { operation: 'count_rows', output: 'count' }] })).toContain('operations[1]');
  });
  it('mirrors the per-type limit keys and the values ceiling', () => {
    expect(fields({ ...split, limits: { values: 32_768 } })).toEqual([]);
    expect(fields({ ...split, limits: { values: 32_769 } })).toContain('limits.values');
    expect(fields({ ...split, limits: { groups: 5 } })).toContain('limits.groups');
    expect(fields({ ...aggregate, limits: { groups: 1_000, values: 32_768 } })).toEqual([]);
    expect(fields({ ...aggregate, limits: { groups: 1_001 } })).toContain('limits.groups');
  });
  const regrouped = { ...aggregate, layout: 'split_out', regroup: 'parent',
    operations: [{ operation: 'collect', output: 'items', field: { path: '/items' } }] };
  it('admits regroup parent only with split_out layout, no group_by and no collect_rows', () => {
    expect(fields(regrouped)).toEqual([]);
    expect(fields({ ...regrouped, regroup: 'none' })).toEqual([]);
    expect(fields({ ...regrouped, regroup: 'child' })).toContain('regroup');
    expect(fields({ ...regrouped, layout: 'plain' })).toContain('regroup');
    expect(fields({ ...regrouped, layout: undefined })).toContain('regroup');
    expect(fields({ ...regrouped, group_by: [{ path: '/a', output: 'a' }] })).toContain('group_by');
    expect(fields({ ...regrouped, group_by: [] })).toEqual([]);
    expect(fields({ ...regrouped, operations: [{ operation: 'collect_rows', output: 'rows' }] })).toContain('operations[0].operation');
  });
  it.each(['sum_int', 'min_int', 'max_int'])('refuses null keep for %s but not for list operations', (operation) => {
    const field = { path: '/x', null: 'keep' };
    expect(fields({ ...aggregate, operations: [{ operation, output: 'v', field }] })).toContain('operations[0].field.null');
    expect(fields({ ...aggregate, operations: [{ operation, output: 'v', field: { path: '/x', null: 'skip' } }] })).toEqual([]);
    expect(fields({ ...aggregate, operations: [{ operation: 'first', output: 'v', field }] })).toEqual([]);
  });
  it('refuses collect_rows retain none', () => {
    const rows = (retain: unknown) => ({ ...aggregate, operations: [{ operation: 'collect_rows', output: 'r', retain }] });
    expect(fields(rows({ mode: 'none' }))).toContain('operations[0].retain.mode');
    for (const mode of ['all', 'only', 'except']) expect(fields(rows({ mode, ...(mode === 'all' ? {} : { fields: [] }) }))).toEqual([]);
  });
  it('validates pointers like the Worker', () => {
    const at = (path: string) => fields({ ...split, source: 'row', split: { mode: 'row_field', path } });
    expect(at('/' + Array.from({ length: 31 }, () => 'a').join('/'))).toEqual([]);
    expect(at('/' + Array.from({ length: 32 }, () => 'a').join('/'))).toEqual([]);
    expect(at('/' + Array.from({ length: 33 }, () => 'a').join('/'))).toContain('split.path');
    expect(at('/' + 'a'.repeat(511))).toEqual([]);
    expect(at('/' + 'a'.repeat(512))).toContain('split.path');
    for (const bad of ['/a~2', '/a~', 'a/b', '']) expect(at(bad)).toContain('split.path');
    expect(at('/a~0b/~1/')).toEqual([]);
  });
  it('cites the Worker definitions', () => {
    const citation = (node: unknown) => graphShapingIssues(documentFor({ ...(node as object), guessed: 1 }))[0]?.citation;
    expect(citation(split)).toBe('split_out.rs::SplitOutNodeDefinition::from_yaml');
    expect(citation(aggregate)).toBe('aggregate.rs::AggregateNodeDefinition::from_yaml');
  });
  it.each(['collect', 'sum_int', 'min_int', 'max_int', 'first', 'last'])('accepts explicit %s selectors with unknown values', (operation) => {
    expect(fields({ ...aggregate, operations: [{ operation, output: 'value', field: { path: '/opaque', missing: 'skip', null: 'error' } }] })).toEqual([]);
  });
});
