import { load } from 'js-yaml';
import { describe, expect, it } from 'vitest';

import { dumpYaml, serializePipelineYaml } from './dumpYaml.helpers';
import { readPipelineStateOrder, renamePipelineStateOrder } from './pipelineYamlState.helpers';

describe('dumpYaml', () => {
  it('round-trips a plain object through YAML', () => {
    const input = { state: { input: { type: 'str' } }, nodes: [{ id: 'n1', type: 'tool' }] };
    const output = dumpYaml(input);
    expect(load(output)).toEqual(input);
  });

  it('reorders a node object so id comes first, then type', () => {
    const output = dumpYaml({ nodes: [{ transition: 'x', id: 'n1', type: 'tool', name: 'Foo' }] });
    const idLine = output.indexOf('id:');
    const typeLine = output.indexOf('type:');
    const nameLine = output.indexOf('name:');
    expect(idLine).toBeGreaterThanOrEqual(0);
    expect(idLine).toBeLessThan(typeLine);
    expect(typeLine).toBeLessThan(nameLine);
  });

  it('orders top-level keys as state -> entry_point -> interrupt_after -> interrupt_before -> nodes', () => {
    const output = dumpYaml({
      nodes: [],
      interrupt_before: [],
      entry_point: 'a',
      state: {},
      interrupt_after: [],
    });
    const order = ['state', 'entry_point', 'interrupt_after', 'interrupt_before', 'nodes'].map((key) =>
      output.indexOf(`${key}:`),
    );
    expect(order).toEqual([...order].sort((a, b) => a - b));
  });

  it('sorts unrecognised top-level keys alphabetically after the known ones', () => {
    const output = dumpYaml({ zeta: 1, state: {}, alpha: 2 });
    const stateIdx = output.indexOf('state:');
    const alphaIdx = output.indexOf('alpha:');
    const zetaIdx = output.indexOf('zeta:');
    expect(stateIdx).toBeLessThan(alphaIdx);
    expect(alphaIdx).toBeLessThan(zetaIdx);
  });

  it('does not wrap long lines (lineWidth: -1)', () => {
    const longValue = 'x'.repeat(500);
    const output = dumpYaml({ instructions: longValue });
    const line = output.split('\n').find((l) => l.includes(longValue));
    expect(line).toBeDefined();
  });

  it('drops non-serializable values (functions/symbols) instead of throwing', () => {
    const output = dumpYaml({ state: {}, fn: () => 1, sym: Symbol('x'), keep: 'yes' });
    expect(output).toContain('keep:');
    expect(output).not.toContain('fn:');
    expect(output).not.toContain('sym:');
  });

  it('handles arrays of nodes, reordering each element', () => {
    const output = dumpYaml([{ b: 1, id: 'n1', type: 'tool' }]);
    const idLine = output.indexOf('id:');
    const bLine = output.indexOf('b:');
    expect(idLine).toBeLessThan(bLine);
  });

  it('returns a non-throwing error string when dumping fails', () => {
    const circular: Record<string, unknown> = {};
    circular['self'] = circular;
    const output = dumpYaml(circular);
    expect(output).toContain('Error dumping YAML:');
  });

  it('passes through primitives and null unchanged', () => {
    expect(load(dumpYaml('hello'))).toBe('hello');
    expect(load(dumpYaml(42))).toBe(42);
    expect(load(dumpYaml(null))).toBeNull();
  });
});

describe('pipeline compatibility serialization', () => {
  const source =
    '# Original spelling\r\nstate:\r\n  "10": list\r\n  "2": {type: dict, value: null, future: {unknown: true}}\r\n  input: str\r\nnodes: []\r\n';

  it('returns the exact original bytes for no-op and layout-only instructions', () => {
    expect(serializePipelineYaml(load(source), { originalYaml: source })).toBe(source);
    expect(dumpYaml(load(source), { originalYaml: source })).toBe(source);
  });

  it('preserves state order and unknown/default/null forms during a real edit', () => {
    const parsed = load(source) as Record<string, unknown>;
    const edited = { ...parsed, entry_point: 'new_entry' };
    const result = serializePipelineYaml(edited, { originalYaml: source });
    expect(load(result)).toEqual(edited);
    expect(readPipelineStateOrder(result)).toEqual(['10', '2', 'input']);
    expect((load(result) as { state: Record<string, unknown> }).state['10']).toBe('list');
  });

  it('preserves explicit non-numeric declaration order without a source snapshot', () => {
    const result = serializePipelineYaml({
      state: { zeta: 'list', alpha: 'dict' },
    });
    expect(readPipelineStateOrder(result)).toEqual(['zeta', 'alpha']);
  });

  it('keeps state roots named id and type in their explicit declaration order', () => {
    const text = 'state: {type: str, id: str}\nnodes: []\n';
    const parsed = load(text) as Record<string, unknown>;
    expect(
      readPipelineStateOrder(serializePipelineYaml({ ...parsed, entry_point: 'n' }, { originalYaml: text })),
    ).toEqual(['type', 'id']);
  });

  it('renames a quoted numeric root at the supplied original ordinal', () => {
    const parsed = load(source) as { state: Record<string, unknown> };
    const state: Record<string, unknown> = {
      ...parsed.state,
      renamed: parsed.state['10'],
    };
    delete state['10'];
    const result = serializePipelineYaml(
      { ...parsed, state },
      {
        originalYaml: source,
        stateKeyOrder: renamePipelineStateOrder(readPipelineStateOrder(source), '10', 'renamed'),
      },
    );
    expect(readPipelineStateOrder(result)).toEqual(['renamed', '2', 'input']);
  });

  it('preserves prototype-like unknown fields as own data properties', () => {
    const text = 'state: {payload: {type: dict, value: {"__proto__": {name: keep}}}}\nnodes: []';
    const parsed = load(text) as Record<string, unknown>;
    expect(load(serializePipelineYaml({ ...parsed, entry_point: 'n' }, { originalYaml: text }))).toEqual({
      ...parsed,
      entry_point: 'n',
    });
  });

  it('preserves multiline strings and heterogeneous JSON values during a content edit', () => {
    const text =
      'state:\n  payload: {type: list, value: [{name: first}, null, 2, last]}\n  text: {type: str, value: "one\\ntwo"}\nnodes: []';
    const parsed = load(text) as Record<string, unknown>;
    expect(load(serializePipelineYaml({ ...parsed, entry_point: 'n' }, { originalYaml: text }))).toEqual({
      ...parsed,
      entry_point: 'n',
    });
  });

  it('refuses a mismatched explicit sequence through the strict write API', () => {
    expect(() =>
      serializePipelineYaml(load(source), {
        originalYaml: source,
        stateKeyOrder: ['2'],
      }),
    ).toThrow('does not match');
  });
});
