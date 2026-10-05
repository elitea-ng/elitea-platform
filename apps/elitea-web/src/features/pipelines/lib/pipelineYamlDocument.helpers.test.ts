import { describe, expect, it } from 'vitest';
import { load } from 'js-yaml';

import {
  parsePipelineYamlDocument,
  pipelineYamlDocumentsEqual,
  preparePipelineYamlEdit,
} from './pipelineYamlDocument.helpers';
import { patchStateVariableMap } from './stateVariableSpec.helpers';

const SOURCE =
  '# original\r\nstate:\r\n  "2": list\r\n  "10": {type: dict, value: null, unknown: {keep: [1, null]}}\r\n  absent: {type: str}\r\n  nullable: null\r\nnodes: []\r\n';

describe('pipeline document caller boundary', () => {
  it('reads quoted numeric roots from YAML syntax and retains every raw declaration', () => {
    const parsed = parsePipelineYamlDocument(SOURCE);
    expect(parsed.stateKeyOrder).toEqual(['2', '10', 'absent', 'nullable']);
    expect(parsed.yamlJsonObject).toEqual(load(SOURCE));
    expect(parsed.yamlCode).toBe(SOURCE);
  });

  it('retains exact original bytes for an unchanged object and cosmetic object-key reorder', () => {
    const parsed = parsePipelineYamlDocument(SOURCE);
    expect(preparePipelineYamlEdit(parsed, parsed.yamlJsonObject).yamlCode).toBe(SOURCE);
    const next = { nodes: [], state: parsed.yamlJsonObject['state'] };
    expect(preparePipelineYamlEdit(parsed, next).yamlCode).toBe(SOURCE);
  });

  it('retains blank/null documents on no-op without inventing an empty mapping', () => {
    for (const source of ['', '# blank\n', 'null\n']) {
      const parsed = parsePipelineYamlDocument(source);
      expect(preparePipelineYamlEdit(parsed, {}).yamlCode).toBe(source);
    }
  });

  it('preserves raw state forms, unknown values and meaningful order on a real node edit', () => {
    const parsed = parsePipelineYamlDocument(SOURCE);
    const next = { ...parsed.yamlJsonObject, nodes: [{ id: 'node', type: 'printer', template: 'hello' }] };
    const saved = preparePipelineYamlEdit(parsed, next);
    expect(load(saved.yamlCode)).toEqual(next);
    expect(parsePipelineYamlDocument(saved.yamlCode).stateKeyOrder).toEqual(parsed.stateKeyOrder);
    expect((load(saved.yamlCode) as { state: unknown }).state).toEqual((load(SOURCE) as { state: unknown }).state);
  });

  it('detects a meaningful order change even when the numeric-key objects are equal', () => {
    const first = parsePipelineYamlDocument('state: {"2": list, "10": dict}\n');
    const second = parsePipelineYamlDocument('state: {"10": dict, "2": list}\n');
    expect(first.yamlJsonObject).toEqual(second.yamlJsonObject);
    expect(pipelineYamlDocumentsEqual(first, second)).toBe(false);
    const changed = preparePipelineYamlEdit(first, first.yamlJsonObject, { stateKeyOrder: second.stateKeyOrder });
    expect(parsePipelineYamlDocument(changed.yamlCode).stateKeyOrder).toEqual(['10', '2']);
  });

  it('retains the ordinal and raw bare entry on rename, including a quoted numeric root', () => {
    const parsed = parsePipelineYamlDocument(SOURCE);
    const state = parsed.yamlJsonObject['state'] as Record<string, unknown>;
    const change = patchStateVariableMap(state, '2', { newName: 'renamed' });
    const saved = preparePipelineYamlEdit(
      parsed,
      { ...parsed.yamlJsonObject, state: change.state },
      { stateRename: change.stateRename! },
    );
    expect(parsePipelineYamlDocument(saved.yamlCode).stateKeyOrder).toEqual(['renamed', '10', 'absent', 'nullable']);
    expect((load(saved.yamlCode) as { state: Record<string, unknown> }).state['renamed']).toBe('list');
  });

  it('rejects malformed root text, missing order, duplicate order and rename collisions', () => {
    expect(() => parsePipelineYamlDocument('[1, 2]')).toThrow('mapping');
    expect(() => parsePipelineYamlDocument('state: {2: list}')).toThrow('strings');
    const parsed = parsePipelineYamlDocument(SOURCE);
    expect(() => preparePipelineYamlEdit(parsed, parsed.yamlJsonObject, { stateKeyOrder: ['2'] })).toThrow('order');
    expect(() =>
      preparePipelineYamlEdit(parsed, parsed.yamlJsonObject, { stateKeyOrder: ['2', '2', 'absent', 'nullable'] }),
    ).toThrow('order');
    expect(() =>
      preparePipelineYamlEdit(parsed, parsed.yamlJsonObject, { stateRename: { from: '2', to: '10' } }),
    ).toThrow('rename');
  });
});

it('retains supported non-JSON YAML scalar metadata through a real edit', () => {
  const source = 'state: {"2": list, "10": dict}\nmeta: {nan: .nan, positive: .inf, when: 2026-10-04}\nnodes: []\n';
  const parsed = parsePipelineYamlDocument(source);
  const next = { ...parsed.yamlJsonObject, nodes: [{ id: 'real', type: 'printer' }] };
  expect(load(preparePipelineYamlEdit(parsed, next).yamlCode)).toEqual(next);
});
