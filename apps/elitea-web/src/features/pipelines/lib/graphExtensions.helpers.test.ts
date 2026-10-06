import { describe, expect, it } from 'vitest';
import { channelDefaultPreview, jsonPointerTokens, orderedDeclaredChannels, patchGraphExtensionNode, pointerDefaultPreview } from './graphExtensions.helpers';
import { GraphExtensionDefaults } from './flow-editor/constants/graphExtensionDefaults.constants';
import { parseNodes } from './flow-editor/helpers/parsePipelineTraversal.helpers';

describe('graph extension compatibility seams', () => {
  it('uses explicit declaration sequence, including quoted integer-like roots', () => {
    expect(orderedDeclaredChannels({ state: { '2': 'dict', '10': 'list', new: 'str' } }, ['10', '2'])).toEqual(['10', '2', 'new']);
  });
  it('preserves unknown node fields and every state declaration when one explicit field changes', () => {
    const state = { '10': 'list', '2': { type: 'dict', value: null }, legacy: { type: 'str', default: null, vendor: true } };
    const old = { state, nodes: [{ id: 's', type: 'split_out', vendor: { any: [null, true] }, source: '10' }] };
    const next = patchGraphExtensionNode(old, 's', 'destination', 'item');
    expect(next.state).toBe(state);
    expect(next.nodes?.[0]?.['vendor']).toEqual({ any: [null, true] });
    expect(old.nodes[0]).not.toHaveProperty('destination');
  });
  it('removes only the requested own field without publishing undefined', () => {
    expect(patchGraphExtensionNode({ nodes: [{ id: 'w', type: 'state_modifier', transition: 'END', template: 'x' }] },
      'w', 'transition', undefined, true).nodes?.[0]).toEqual({ id: 'w', type: 'state_modifier', template: 'x' });
  });
  it.each([['/a~1b/~0key', ['a/b', '~key']], ['/', ['']], ['/00', ['00']]])('decodes exact pointer %s', (source, expected) => {
    expect(jsonPointerTokens(source)).toEqual(expected);
  });
  it.each(['', 'root.field', '/bad~2escape', '/bad~', '/' + '界'.repeat(172), '/a'.repeat(33)])('refuses invalid or unbounded pointer %s', (source) => {
    expect(jsonPointerTokens(source)).toBeUndefined();
  });
  it('previews only an authored default and never treats bare/default metadata as inferred shape', () => {
    expect(channelDefaultPreview('list')).toBe('');
    expect(channelDefaultPreview({ type: 'dict', default: { x: 1 } })).toBe('');
    expect(pointerDefaultPreview({ type: 'dict', value: { 'a/b': [null] } }, '/a~1b/0')).toBe('null');
    expect(pointerDefaultPreview({ value: { 'a/b': [null] } }, '/a~1b/00')).toBe('');
  });
  it('bounds previews of deep or cyclic defaults without creating shape metadata', () => {
    const cyclic: Record<string, unknown> = { value: 'x' };
    cyclic['self'] = cyclic;
    expect(channelDefaultPreview({ value: cyclic }).length).toBeLessThanOrEqual(161);
    expect(channelDefaultPreview({ value: Array.from({ length: 10_000 }, () => 'x'.repeat(500)) }).length).toBeLessThanOrEqual(161);
  });
  it.each(['map', 'split_out', 'aggregate'])('parses stored %s as its dedicated node with transition', (type) => {
    const parsed = parseNodes({ entry_point: 'n', nodes: [{ id: 'n', type, ...GraphExtensionDefaults[type] }] });
    expect(parsed.nodes.find((node) => node.id === 'n')?.type).toBe(type);
    expect(parsed.edges.some((edge) => edge.source === 'n' && edge.target === 'END')).toBe(true);
  });
});
