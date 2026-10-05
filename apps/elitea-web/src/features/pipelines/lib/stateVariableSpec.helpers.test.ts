import { describe, expect, it } from 'vitest';

import {
  patchStateVariableMap,
  patchStateVariableSpec,
  readStateVariableSpec,
  stateVariableSpecsForDisplay,
} from './stateVariableSpec.helpers';

describe('raw state variable adapter', () => {
  it.each(['list', 'dict', 'number', 'string'])('retains the bare %s declaration', (type) => {
    expect(readStateVariableSpec(type)).toEqual({
      rawSpec: type,
      type,
      hasDefault: false,
      defaultValue: undefined,
    });
  });

  it('keeps missing defaults distinct from explicit null', () => {
    expect(readStateVariableSpec({ type: 'dict' }).hasDefault).toBe(false);
    expect(readStateVariableSpec({ type: 'dict', value: null })).toMatchObject({
      hasDefault: true,
      defaultValue: null,
    });
  });

  it('does not coerce an empty or heterogeneous default', () => {
    const value = [{ name: 'first' }, null, 1, 'last'];
    const raw = { type: 'list', value, future: { mode: 'unknown' } };
    expect(readStateVariableSpec(raw)).toMatchObject({
      rawSpec: raw,
      defaultValue: value,
    });
    expect(readStateVariableSpec(raw).rawSpec).toBe(raw);
  });

  it('preserves unknown fields during a selected descriptor edit', () => {
    const raw = { type: 'dict', value: null, future: { items: ['keep'] } };
    expect(patchStateVariableSpec(raw, { description: 'updated' })).toEqual({
      ...raw,
      description: 'updated',
    });
    expect(raw).toEqual({
      type: 'dict',
      value: null,
      future: { items: ['keep'] },
    });
  });

  it('retains bare declarations for empty or same-type patches', () => {
    expect(patchStateVariableSpec('list', {})).toBe('list');
    expect(patchStateVariableSpec('dict', { type: 'dict' })).toBe('dict');
  });

  it('converts only an explicitly edited bare spec and retains its type alias', () => {
    expect(patchStateVariableSpec('number', { value: 2 })).toEqual({
      type: 'number',
      value: 2,
    });
  });

  it.each([{ raw: null }, { raw: [] }, { raw: false }])(
    'keeps a malformed entry readable and refuses form coercion: %j',
    ({ raw }) => {
      expect(readStateVariableSpec(raw).rawSpec).toBe(raw);
      expect(() => patchStateVariableSpec(raw, { type: 'str' })).toThrow('malformed');
    },
  );
});

describe('raw state caller adaptation', () => {
  it('projects bare/default/null entries for display without modifying raw values', () => {
    const raw = {
      bare: 'list',
      absent: { type: 'dict', shape: { future: true } },
      nullable: { type: 'dict', value: null },
      malformed: null,
    };
    expect(stateVariableSpecsForDisplay(raw)).toEqual({
      bare: { type: 'list' },
      absent: { type: 'dict' },
      nullable: { type: 'dict', value: null },
      malformed: {},
    });
    expect(raw.bare).toBe('list');
    expect(Object.hasOwn(raw.absent, 'value')).toBe(false);
  });
  it('renames a bare declaration without expanding it or inserting newName metadata', () => {
    const raw = { first: 'list', last: { type: 'dict', value: null, unknown: 2 } };
    const result = patchStateVariableMap(raw, 'first', { newName: 'renamed' });
    expect(result.state).toEqual({ renamed: 'list', last: raw.last });
    expect(result.stateRename).toEqual({ from: 'first', to: 'renamed' });
    expect(raw).toEqual({ first: 'list', last: { type: 'dict', value: null, unknown: 2 } });
  });
  it('patches only the selected entry and keeps opaque metadata and absent defaults', () => {
    const raw = {
      bare: 'list',
      descriptor: { type: 'dict', value: null, unknown: { keep: true } },
      absent: { type: 'str' },
    };
    expect(patchStateVariableMap(raw, 'descriptor', { value: { x: 1 } }).state).toEqual({
      ...raw,
      descriptor: { type: 'dict', value: { x: 1 }, unknown: { keep: true } },
    });
    expect(patchStateVariableMap(raw, 'bare', { type: 'list' }).state['bare']).toBe('list');
    expect(patchStateVariableMap(raw, 'absent', { newName: 'absent' }).state['absent']).toEqual({ type: 'str' });
  });
  it('rejects an existing target even when its raw value is null and rejects an unknown source', () => {
    expect(() => patchStateVariableMap({ a: 'list', b: null }, 'a', { newName: 'b' })).toThrow('collides');
    expect(() => patchStateVariableMap({ a: 'list' }, 'missing', {})).toThrow('Unknown');
  });
});
