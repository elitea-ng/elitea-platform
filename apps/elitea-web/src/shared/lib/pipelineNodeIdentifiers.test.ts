import { load } from 'js-yaml';
import { describe, expect, it } from 'vitest';

import { normalizePipelineNodeIdentifiers, unsupportedIdentifierText } from './pipelineNodeIdentifiers';

const NUMERIC = `entry_point: 1
interrupt_before: [2]
interrupt_after: [1]
nodes:
  - id: 1
    type: llm
    transition: 2
  - id: 2
    type: decision
    nodes: [3, 4]
    default_output: 3
  - id: 3
    type: router
    routes: [4, 5]
    default_output: 4
  - id: 4
    type: hitl
    routes: {approve: 5, reject: 1, edit: END}
  - id: 5
    type: map
    worker: 6
    transition: 0
  - id: 6
    type: state_modifier
  - id: 7
    type: parallel
    branches:
      - {id: 8, node: 6}
      - {id: b, node: 9}
`;

type Loose = Record<string, any>;

describe('normalizePipelineNodeIdentifiers', () => {
  it('turns every integer graph identifier the editor reads into its canonical decimal string', () => {
    const result = normalizePipelineNodeIdentifiers(load(NUMERIC)) as Loose;
    expect(result['entry_point']).toBe('1');
    expect(result['interrupt_before']).toEqual(['2']);
    expect(result['interrupt_after']).toEqual(['1']);
    const [n1, n2, n3, n4, n5, n6, n7] = result['nodes'] as Loose[];
    expect(n1).toMatchObject({ id: '1', transition: '2' });
    expect(n2).toMatchObject({ id: '2', nodes: ['3', '4'], default_output: '3' });
    expect(n3).toMatchObject({ id: '3', routes: ['4', '5'], default_output: '4' });
    expect(n4).toMatchObject({ id: '4', routes: { approve: '5', reject: '1', edit: 'END' } });
    expect(n5).toMatchObject({ id: '5', worker: '6', transition: '0' });
    expect(n6).toMatchObject({ id: '6' });
    expect(n7?.['branches']).toEqual([
      { id: '8', node: '6' },
      { id: 'b', node: '9' },
    ]);
  });

  it('also covers the legacy inline condition and decision sub-objects', () => {
    const result = normalizePipelineNodeIdentifiers({
      nodes: [
        { id: 1, condition: { conditional_outputs: [2, 'x'], default_output: 3 } },
        { id: 2, decision: { nodes: [1], default_output: 2 } },
      ],
    }) as { nodes: Loose[] };
    expect(result.nodes[0]).toMatchObject({ id: '1', condition: { conditional_outputs: ['2', 'x'], default_output: '3' } });
    expect(result.nodes[1]).toMatchObject({ id: '2', decision: { nodes: ['1'], default_output: '2' } });
  });

  it('returns the very same object when nothing needs normalizing, and never mutates its input', () => {
    const plain = load('entry_point: A\nnodes:\n  - id: A\n    type: llm\n    transition: END\n');
    expect(normalizePipelineNodeIdentifiers(plain)).toBe(plain);

    const numeric = load(NUMERIC);
    const before = JSON.stringify(numeric);
    const result = normalizePipelineNodeIdentifiers(numeric);
    expect(result).not.toBe(numeric);
    expect(JSON.stringify(numeric)).toBe(before);
    expect(normalizePipelineNodeIdentifiers(result)).toBe(result);
  });

  it('leaves values that are not identifiers alone, even when they are numbers', () => {
    const doc = {
      state: { 1: 'str' },
      nodes: [{ id: 'A', type: 'llm', transition: 'END', input_mapping: { task: { type: 'fixed', value: 5 } }, max_items: 3 }],
    };
    expect(normalizePipelineNodeIdentifiers(doc)).toBe(doc);
  });

  it('normalizes only safe integers: everything else is left exactly as authored', () => {
    const unsafe = Number.MAX_SAFE_INTEGER + 1;
    const doc = {
      entry_point: 1.5,
      interrupt_before: [true, null, { a: 1 }, unsafe, -unsafe, Number.NaN, Number.POSITIVE_INFINITY, 7],
      nodes: [
        { id: unsafe, transition: false, routes: [1.5, 2] },
        { id: null, transition: { x: 1 } },
        { id: Number.MAX_SAFE_INTEGER, default_output: -Number.MAX_SAFE_INTEGER },
      ],
    };
    const result = normalizePipelineNodeIdentifiers(doc) as unknown as { entry_point: unknown; interrupt_before: unknown; nodes: unknown[] };
    expect(result.entry_point).toBe(1.5);
    expect(result.interrupt_before).toEqual([true, null, { a: 1 }, unsafe, -unsafe, Number.NaN, Number.POSITIVE_INFINITY, '7']);
    expect(result.nodes[0]).toEqual({ id: unsafe, transition: false, routes: [1.5, '2'] });
    expect(result.nodes[1]).toEqual({ id: null, transition: { x: 1 } });
    expect(result.nodes[2]).toEqual({ id: String(Number.MAX_SAFE_INTEGER), default_output: String(-Number.MAX_SAFE_INTEGER) });
  });

  it('never throws on a malformed document shape', () => {
    const shapes: unknown[] = [undefined, null, 5, 'x', [], { nodes: 'x' }, { nodes: [null, 1, 'a', []] }, { nodes: [{ routes: 3 }] }, { nodes: [{ type: 'parallel', branches: [null, 1, 'a'] }] }];
    for (const doc of shapes) expect(() => normalizePipelineNodeIdentifiers(doc)).not.toThrow();
    expect(normalizePipelineNodeIdentifiers(null)).toBeNull();
    expect(normalizePipelineNodeIdentifiers(undefined)).toBeUndefined();
  });
});

describe('unsupportedIdentifierText', () => {
  it('names the type and value, and always contains a space so it can never pass the id grammar', () => {
    expect(unsupportedIdentifierText(1.5)).toBe('number 1.5');
    expect(unsupportedIdentifierText(true)).toBe('boolean true');
    expect(unsupportedIdentifierText(Number.MAX_SAFE_INTEGER + 1)).toBe('number 9007199254740992');
    expect(unsupportedIdentifierText({ a: 1 })).toBe('object');
    expect(unsupportedIdentifierText([1])).toBe('list');
  });
});
