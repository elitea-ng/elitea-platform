import { load } from 'js-yaml';
import { describe, expect, it } from 'vitest';

import { PIPELINE_165_YAML } from '../__tests__/pipeline165Fixture';
import { dumpYaml, serializePipelineYaml, trySerializePipelineYaml } from './dumpYaml.helpers';
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

  it.each(['', ' \n', '# comment only\n'])('retains an empty original document and permits its first edit: %j', (originalYaml) => {
    expect(serializePipelineYaml({}, { originalYaml })).toBe(originalYaml);
    const next = { entry_point: 'LLM_1', nodes: [{ id: 'LLM_1', type: 'llm', transition: 'END' }] };
    expect(load(serializePipelineYaml(next, { originalYaml }))).toEqual(next);
  });

  it.each(['state: [', 'nodes: []\n---\nnodes: []\n'])('refuses malformed or multiple original documents: %j', (originalYaml) => {
    expect(() => serializePipelineYaml({ nodes: [] }, { originalYaml })).toThrow();
  });

  it('keeps explicit null distinct from an empty document', () => {
    expect(serializePipelineYaml(null, { originalYaml: 'null' })).toBe('null');
    expect(load(serializePipelineYaml({}, { originalYaml: 'null' }))).toEqual({});
  });

  it('retains strict roundtrip refusal for an unrelated undefined field in the first edit', () => {
    expect(() => serializePipelineYaml({ nodes: [], opaque: undefined }, { originalYaml: '' })).toThrow(
      'Pipeline YAML serialization changed its contract',
    );
  });

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

/** Pipeline 165 as the flow editor held it before the fix: the `ls` defaults each carried `enum: undefined`. */
function pipeline165WithUndefinedEnum(): Record<string, unknown> {
  const document = load(PIPELINE_165_YAML) as { nodes: Record<string, unknown>[] };
  const empty = { type: 'fixed', value: null, enum: undefined };
  document.nodes[1] = {
    ...document.nodes[1],
    input_mapping: { skip: empty, recursive: { type: 'fixed', value: false, enum: undefined }, include: empty, folder: empty, bucket_name: empty },
  };
  return document;
}

describe('pipeline 165 round-trip', () => {
  it('keeps the stored text byte for byte when the editor holds the stored document', () => {
    expect(serializePipelineYaml(load(PIPELINE_165_YAML), { originalYaml: PIPELINE_165_YAML })).toBe(PIPELINE_165_YAML);
    expect(load(serializePipelineYaml(load(PIPELINE_165_YAML)))).toStrictEqual(load(PIPELINE_165_YAML));
  });

  it('keeps empty input_mapping and dict state types through a real edit', () => {
    const edited = { ...(load(PIPELINE_165_YAML) as Record<string, unknown>), entry_point: 'ls' };
    const text = serializePipelineYaml(edited, { originalYaml: PIPELINE_165_YAML });
    expect(load(text)).toStrictEqual(edited);
    expect(text).toContain('input_mapping: {}');
    expect(readPipelineStateOrder(text)).toEqual(['input', 'messages', 'created', 'listing']);
  });

  it('names the member YAML cannot hold instead of a bare contract error', () => {
    expect(() => serializePipelineYaml(pipeline165WithUndefinedEnum())).toThrow(
      'Pipeline YAML serialization changed its contract: nodes[1].input_mapping.skip.enum has no YAML form',
    );
  });

  it('names a date member, which the default schema loads back as a string', () => {
    expect(() => serializePipelineYaml({ ...(load(PIPELINE_165_YAML) as object), at: new Date('2026-10-09T00:00:00Z') })).toThrow(
      'Pipeline YAML serialization changed its contract: at has no YAML form',
    );
  });

  it('reports the refusal as a value, so a caller never stores the error text as YAML', () => {
    const result = trySerializePipelineYaml(pipeline165WithUndefinedEnum());
    expect(result.yaml).toBeUndefined();
    expect(result.error).toContain('nodes[1].input_mapping.skip.enum');
    expect(trySerializePipelineYaml(load(PIPELINE_165_YAML)).error).toBeUndefined();
  });
});
