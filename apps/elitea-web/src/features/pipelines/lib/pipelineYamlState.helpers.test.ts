import { load } from 'js-yaml';
import { describe, expect, it } from 'vitest';

import {
  pipelineYamlFingerprint,
  pipelineValueFingerprint,
  readPipelineStateOrder,
  reconcilePipelineStateOrder,
  renamePipelineStateOrder,
  validatePipelineStateOrder,
} from './pipelineYamlState.helpers';

const source = 'state:\n  "10": list\n  "2": {type: dict, value: null}\n  input: str\n';

describe('pipeline state declaration order', () => {
  it('reads quoted integer-like roots before object enumeration loses their order', () => {
    expect(Object.keys((load(source) as { state: object }).state)).toEqual(['2', '10', 'input']);
    expect(readPipelineStateOrder(source)).toEqual(['10', '2', 'input']);
  });

  it('reads flow mappings, escaped keys, and dotted roots as literal strings', () => {
    expect(readPipelineStateOrder('state: {"a.b": list, "2": dict, "a\\u0020b": str}\n')).toEqual(['a.b', '2', 'a b']);
  });

  it('resolves an anchored state mapping without flattening the source', () => {
    const text = 'schema: &state_defs {"10": list, "2": dict}\nstate: *state_defs\n';
    expect(readPipelineStateOrder(text)).toEqual(['10', '2']);
  });

  it('returns an empty order when there is no state mapping', () => {
    expect(readPipelineStateOrder('nodes: []')).toEqual([]);
    expect(readPipelineStateOrder('')).toEqual([]);
  });

  it('rejects non-string state keys instead of converting them to quoted strings', () => {
    expect(() => readPipelineStateOrder('state: {2: list}')).toThrow('keys must be strings');
  });

  it('rejects multiple YAML documents', () => {
    expect(() => readPipelineStateOrder('state: {}\n---\nstate: {}')).toThrow('one pipeline');
  });

  it('retains surviving keys and appends only new declarations', () => {
    expect(
      reconcilePipelineStateOrder({ state: { '10': 'list', '2': 'dict', added: 'str' } }, ['10', '2', 'removed']),
    ).toEqual(['10', '2', 'added']);
  });

  it('renames a key at the original ordinal', () => {
    expect(renamePipelineStateOrder(['10', '2', 'input'], '10', 'renamed')).toEqual(['renamed', '2', 'input']);
    expect(() => renamePipelineStateOrder(['10', '2'], '10', '2')).toThrow('rename');
  });

  it.each([['10'], ['10', '10', '2'], ['10', 'missing']])('rejects invalid explicit order %j', (...order) => {
    expect(() => validatePipelineStateOrder({ state: { '10': 'list', '2': 'dict' } }, order)).toThrow('does not match');
  });

  it('counts a state reorder as a semantic contract change', () => {
    const reordered = 'state:\n  "2": {type: dict, value: null}\n  "10": list\n  input: str\n';
    expect(pipelineYamlFingerprint(source)).not.toBe(pipelineYamlFingerprint(reordered));
  });

  it('ignores cosmetic descriptor and top-level key order', () => {
    expect(pipelineYamlFingerprint('state: {result: {type: dict, value: {b: 2, a: 1}}}\nnodes: []')).toBe(
      pipelineYamlFingerprint('nodes: []\nstate: {result: {value: {a: 1, b: 2}, type: dict}}'),
    );
  });

  it('distinguishes missing defaults, null, and a bare declaration', () => {
    const bare = pipelineYamlFingerprint('state: {result: dict}');
    const absent = pipelineYamlFingerprint('state: {result: {type: dict}}');
    const explicitNull = pipelineYamlFingerprint('state: {result: {type: dict, value: null}}');
    expect(new Set([bare, absent, explicitNull]).size).toBe(3);
  });
});

describe('complete semantic values', () => {
  it('does not collapse non-finite numbers or missing/undefined/null fields', () => {
    const values = [NaN, Infinity, -Infinity, null, undefined].map((value) => pipelineValueFingerprint({ value }, []));
    expect(new Set(values).size).toBe(5);
    expect(pipelineValueFingerprint({}, [])).not.toBe(pipelineValueFingerprint({ value: undefined }, []));
  });
  it('distinguishes dates and binary values from ordinary objects', () => {
    expect(pipelineValueFingerprint(new Date('2026-10-04T00:00:00Z'), [])).not.toBe(pipelineValueFingerprint({}, []));
    expect(pipelineValueFingerprint(new Uint8Array([1, 2]), [])).not.toBe(
      pipelineValueFingerprint({ '0': 1, '1': 2 }, []),
    );
  });
  it('rejects values that cannot safely round-trip instead of ignoring them', () => {
    expect(() => pipelineValueFingerprint({ fn: () => 1 }, [])).toThrow('Unsupported');
    expect(() => pipelineValueFingerprint(new Map([['a', 1]]), [])).toThrow('Unsupported');
    const cyclic: Record<string, unknown> = {};
    cyclic['self'] = cyclic;
    expect(() => pipelineValueFingerprint(cyclic, [])).toThrow('Cyclic');
  });
});
