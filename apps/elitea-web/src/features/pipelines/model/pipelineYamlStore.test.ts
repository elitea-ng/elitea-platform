import { beforeEach, describe, expect, it } from 'vitest';

import { load } from 'js-yaml';
import { parsePipelineYamlDocument } from '../lib/pipelineYamlDocument.helpers';
import { patchStateVariableMap } from '../lib/stateVariableSpec.helpers';
import { createPipelineYamlStore, usePipelineYamlStore } from './pipelineYamlStore';

const INITIAL_STATE = {
  yamlCode: '',
  yamlJsonObject: {},
  initYamlCode: '',
  initYamlJsonObject: {},
  stateKeyOrder: [],
  initStateKeyOrder: [],
  resetFlag: false,
  layoutVersion: undefined,
};

beforeEach(() => {
  // Full reset for test isolation — the singleton persists across tests,
  // and `resetPipelineYaml()` itself now restores to the (test-polluted)
  // init snapshot rather than blanking it (see the store's own doc comment
  // on that behaviour correction), so it cannot be used for test cleanup.
  usePipelineYamlStore.setState(INITIAL_STATE);
});

describe('createPipelineYamlStore', () => {
  it('starts with empty yamlCode/yamlJsonObject/initYamlCode/initYamlJsonObject, resetFlag false, layoutVersion undefined', () => {
    const store = createPipelineYamlStore();
    expect(store.getState()).toMatchObject(INITIAL_STATE);
  });

  it('setYamlCode replaces the current code without touching initYamlCode', () => {
    const store = createPipelineYamlStore();
    store.getState().setYamlCode('nodes: []');
    expect(store.getState().yamlCode).toBe('nodes: []');
    expect(store.getState().initYamlCode).toBe('');
  });

  it('setYamlJsonObject replaces the object (shallow), matching the baseline reducer', () => {
    const store = createPipelineYamlStore();
    store.getState().setYamlJsonObject({ nodes: [{ id: 'a' }] });
    expect(store.getState().yamlJsonObject).toEqual({ nodes: [{ id: 'a' }] });
  });

  it('initPipelineYaml seeds both the current and the saved snapshot (yamlCode + yamlJsonObject)', () => {
    const store = createPipelineYamlStore();
    store.getState().initPipelineYaml({ yamlCode: 'state: {}', yamlJsonObject: { state: {} } });
    expect(store.getState().yamlCode).toBe('state: {}');
    expect(store.getState().yamlJsonObject).toEqual({ state: {} });
    expect(store.getState().initYamlCode).toBe('state: {}');
    expect(store.getState().initYamlJsonObject).toEqual({ state: {} });
  });

  it('initPipelineYaml also sets resetFlag (baseline initThePipeline reducer parity — every version load/reload must re-sync the flow-editor canvas, matching resetPipelineYaml)', () => {
    const store = createPipelineYamlStore();
    expect(store.getState().resetFlag).toBe(false);

    store.getState().initPipelineYaml({ yamlCode: 'state: {}', yamlJsonObject: { state: {} } });

    expect(store.getState().resetFlag).toBe(true);
  });

  it('initPipelineYaml sets resetFlag even on a RELOAD (a second call while the editor stayed mounted across a version switch)', () => {
    const store = createPipelineYamlStore();
    store.getState().initPipelineYaml({ yamlCode: 'a: 1', yamlJsonObject: { a: 1 } });
    store.getState().clearResetFlag();
    expect(store.getState().resetFlag).toBe(false);

    store.getState().initPipelineYaml({ yamlCode: 'b: 2', yamlJsonObject: { b: 2 } });

    expect(store.getState().resetFlag).toBe(true);
  });

  it('markYamlCodeSaved snapshots the saved text, parsed document and declaration order together', () => {
    const store = createPipelineYamlStore();
    store.getState().initPipelineYaml({ yamlCode: 'a: 1', yamlJsonObject: { a: 1 } });
    store.getState().setYamlCode('a: 2');
    store.getState().markYamlCodeSaved();
    expect(store.getState().initYamlCode).toBe('a: 2');
    expect(store.getState().yamlJsonObject).toEqual({ a: 2 });
    expect(store.getState().initYamlJsonObject).toEqual({ a: 2 });
  });

  it('resetPipelineYaml restores yamlCode/yamlJsonObject from the last init snapshot (NOT to blank) and sets resetFlag', () => {
    const store = createPipelineYamlStore();
    store.getState().initPipelineYaml({ yamlCode: 'a: 1', yamlJsonObject: { a: 1 } });
    store.getState().setYamlCode('a: 2 (unsaved edit)');
    store.getState().setYamlJsonObject({ a: 2 });

    store.getState().resetPipelineYaml();

    expect(store.getState().yamlCode).toBe('a: 1');
    expect(store.getState().yamlJsonObject).toEqual({ a: 1 });
    expect(store.getState().resetFlag).toBe(true);
  });

  it('resetPipelineYaml on a never-initialised store restores to blank (the init snapshot itself is still blank)', () => {
    const store = createPipelineYamlStore();
    store.getState().setYamlCode('scratch');
    store.getState().resetPipelineYaml();
    expect(store.getState().yamlCode).toBe('');
    expect(store.getState().yamlJsonObject).toEqual({});
  });

  it('clearResetFlag flips resetFlag back to false', () => {
    const store = createPipelineYamlStore();
    store.getState().resetPipelineYaml();
    expect(store.getState().resetFlag).toBe(true);
    store.getState().clearResetFlag();
    expect(store.getState().resetFlag).toBe(false);
  });

  it('setLayoutVersion sets layoutVersion', () => {
    const store = createPipelineYamlStore();
    store.getState().setLayoutVersion('v2');
    expect(store.getState().layoutVersion).toBe('v2');
  });

  it('two independently-created stores do not share state', () => {
    const storeA = createPipelineYamlStore();
    const storeB = createPipelineYamlStore();
    storeA.getState().setYamlCode('only-on-a');
    expect(storeB.getState().yamlCode).toBe('');
  });
});

describe('usePipelineYamlStore (lazy singleton)', () => {
  it('getState/setState operate on the same underlying instance the hook selector reads', () => {
    usePipelineYamlStore.setState({ yamlCode: 'hello: world' });
    expect(usePipelineYamlStore.getState().yamlCode).toBe('hello: world');
  });
});

describe('saved YAML and state sequence integration', () => {
  const source =
    '# source\r\nstate:\r\n  "2": list\r\n  "10": {type: dict, value: null, opaque: {flag: true}}\r\n  absent: {type: str}\r\n  nullable: null\r\nnodes: []\r\n';
  function loadedStore() {
    const store = createPipelineYamlStore();
    store.getState().initPipelineYaml(parsePipelineYamlDocument(source));
    return store;
  }
  it('keeps original bytes through parse, no-op form commit, layout-only changes and save/discard', () => {
    const store = loadedStore();
    expect(store.getState().parsePipelineYamlCode(source)).toBe(false);
    store.getState().editPipelineYamlDocument({ ...store.getState().yamlJsonObject });
    store.getState().setLayoutVersion('layout-v2');
    store.getState().markYamlCodeSaved();
    store.getState().resetPipelineYaml();
    expect(store.getState().yamlCode).toBe(source);
    expect(store.getState().stateKeyOrder).toEqual(['2', '10', 'absent', 'nullable']);
  });
  it('publishes all three document fields once on a semantic edit and preserves every state declaration', () => {
    const store = loadedStore();
    const snapshots: unknown[] = [];
    store.subscribe((state) =>
      snapshots.push({ code: state.yamlCode, object: state.yamlJsonObject, order: state.stateKeyOrder }),
    );
    const next = { ...store.getState().yamlJsonObject, nodes: [{ id: 'p', type: 'printer', template: 'changed' }] };
    store.getState().editPipelineYamlDocument(next);
    expect(snapshots).toHaveLength(1);
    expect(load(store.getState().yamlCode)).toEqual(next);
    expect(parsePipelineYamlDocument(store.getState().yamlCode).stateKeyOrder).toEqual([
      '2',
      '10',
      'absent',
      'nullable',
    ]);
    expect(store.getState().stateKeyOrder).toEqual(['2', '10', 'absent', 'nullable']);
  });
  it('keeps the complete store unchanged when serialization or order validation fails', () => {
    const store = loadedStore();
    const before = store.getState();
    const next = { ...before.yamlJsonObject, unsupported: () => 1 };
    expect(() => store.getState().editPipelineYamlDocument(next)).toThrow();
    expect(store.getState()).toBe(before);
    expect(() => store.getState().editPipelineYamlDocument(before.yamlJsonObject, { stateKeyOrder: ['2'] })).toThrow();
    expect(store.getState()).toBe(before);
  });
  it('keeps invalid raw text in progress and the last valid flow view/order', () => {
    const store = loadedStore();
    store.getState().setYamlCode('state: [');
    const before = store.getState();
    expect(() => store.getState().parsePipelineYamlCode(before.yamlCode)).toThrow();
    expect(store.getState()).toBe(before);
    expect(store.getState().yamlJsonObject).toEqual(load(source));
    expect(store.getState().stateKeyOrder).toEqual(['2', '10', 'absent', 'nullable']);
  });
  it('reads raw YAML reorder as a meaningful edit with the same object values', () => {
    const store = loadedStore();
    const reordered = source.replace(
      '  "2": list\r\n  "10": {type: dict, value: null, opaque: {flag: true}}',
      '  "10": {type: dict, value: null, opaque: {flag: true}}\r\n  "2": list',
    );
    store.getState().setYamlCode(reordered);
    expect(store.getState().parsePipelineYamlCode(reordered)).toBe(true);
    expect(store.getState().yamlCode).toBe(reordered);
    expect(store.getState().yamlJsonObject).toEqual(load(source));
    expect(store.getState().stateKeyOrder).toEqual(['10', '2', 'absent', 'nullable']);
  });
  it('keeps rename ordinal and exact raw entry in actual saved YAML', () => {
    const store = loadedStore();
    const raw = store.getState().yamlJsonObject['state'] as Record<string, unknown>;
    const change = patchStateVariableMap(raw, '2', { newName: 'renamed' });
    store
      .getState()
      .editPipelineYamlDocument(
        { ...store.getState().yamlJsonObject, state: change.state },
        { stateRename: change.stateRename! },
      );
    expect((load(store.getState().yamlCode) as { state: Record<string, unknown> }).state['renamed']).toBe('list');
    expect(parsePipelineYamlDocument(store.getState().yamlCode).stateKeyOrder).toEqual([
      'renamed',
      '10',
      'absent',
      'nullable',
    ]);
  });
  it('discards to the latest saved values and declaration sequence after a later edit', () => {
    const store = loadedStore();
    store
      .getState()
      .editPipelineYamlDocument(
        { ...store.getState().yamlJsonObject, nodes: [{ id: 'saved', type: 'printer' }] },
        { stateKeyOrder: ['10', '2', 'absent', 'nullable'] },
      );
    const savedCode = store.getState().yamlCode;
    store.getState().markYamlCodeSaved();
    store.getState().editPipelineYamlDocument({ ...store.getState().yamlJsonObject, nodes: [] });
    store.getState().resetPipelineYaml();
    expect(store.getState().yamlCode).toBe(savedCode);
    expect(store.getState().yamlJsonObject).toEqual(load(savedCode));
    expect(store.getState().stateKeyOrder).toEqual(['10', '2', 'absent', 'nullable']);
  });
  it('keeps an invalid loaded source editable instead of throwing from version initialization', () => {
    const store = createPipelineYamlStore();
    expect(() => store.getState().initPipelineYaml({ yamlCode: 'state: [', yamlJsonObject: {} })).not.toThrow();
    expect(store.getState().yamlCode).toBe('state: [');
    expect(store.getState().stateKeyOrder).toEqual([]);
  });
  it('refuses stale flow edits until the current valid YAML has been parsed', () => {
    const store = loadedStore();
    store.getState().setYamlCode(source.replace('nodes: []', 'nodes: [{id: newer, type: printer}]'));
    const before = store.getState();
    expect(() => store.getState().editPipelineYamlDocument({ ...before.yamlJsonObject, entry_point: 'old' })).toThrow(
      'Parse current YAML',
    );
    expect(store.getState()).toBe(before);
  });
});
