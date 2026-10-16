import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { dump, load } from 'js-yaml';
import { renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { parsePipelineYamlDocument } from '../pipelineYamlDocument.helpers';
import { usePipelineAttachmentYamlSync } from './usePipelineAttachmentYamlSync.hooks';

let restoreAtomicEdit: (() => void) | undefined;
afterEach(() => { restoreAtomicEdit?.(); restoreAtomicEdit = undefined; });

beforeEach(() => {
  usePipelineYamlStore.setState({
    yamlCode: '',
    yamlJsonObject: {},
    initYamlCode: '',
    initYamlJsonObject: {},
    stateKeyOrder: [],
    initStateKeyOrder: [],
    resetFlag: false,
    layoutVersion: undefined,
  });
});

describe('usePipelineAttachmentYamlSync', () => {
  it('adds input_attachments to state when hasAttachments is true and the key is missing', () => {
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument('state: {input: {type: str}}'));

    renderHook(() => usePipelineAttachmentYamlSync(true));

    const state = usePipelineYamlStore.getState();
    expect(state.yamlJsonObject).toMatchObject({
      state: { input: { type: 'str' }, input_attachments: { type: 'list', value: [] } },
    });
    expect(load(state.yamlCode)).toEqual(state.yamlJsonObject);
  });

  it('seeds the DefaultState when the document has no state key yet', () => {
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument('{}'));

    renderHook(() => usePipelineAttachmentYamlSync(true));

    const state = usePipelineYamlStore.getState();
    expect(state.yamlJsonObject).toMatchObject({
      state: { input: { type: 'str' }, messages: { type: 'list' }, input_attachments: { type: 'list', value: [] } },
    });
  });

  it('removes input_attachments from state when hasAttachments flips to false', () => {
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument('state: {input: {type: str}, input_attachments: {type: list, value: []}}'));

    renderHook(() => usePipelineAttachmentYamlSync(false));

    const state = usePipelineYamlStore.getState();
    expect(state.yamlJsonObject).toEqual({ state: { input: { type: 'str' } } });
  });

  it('does nothing when hasAttachments is true and the key is already present', () => {
    const yamlJsonObject = { state: { input_attachments: { type: 'list', value: [] } } };
    usePipelineYamlStore.getState().initPipelineYaml({yamlJsonObject,yamlCode:dump(yamlJsonObject)});
    const currentDocument = usePipelineYamlStore.getState().yamlJsonObject;

    renderHook(() => usePipelineAttachmentYamlSync(true));

    expect(usePipelineYamlStore.getState().yamlJsonObject).toBe(currentDocument);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(dump(yamlJsonObject));
  });

  it('does nothing when hasAttachments is false and the key is already absent', () => {
    const yamlJsonObject = { state: { input: { type: 'str' } } };
    usePipelineYamlStore.getState().initPipelineYaml({yamlJsonObject,yamlCode:dump(yamlJsonObject)});
    const currentDocument = usePipelineYamlStore.getState().yamlJsonObject;

    renderHook(() => usePipelineAttachmentYamlSync(false));

    expect(usePipelineYamlStore.getState().yamlJsonObject).toBe(currentDocument);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(dump(yamlJsonObject));
  });

  it('reacts to hasAttachments changing across a re-render', () => {
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument('state: {}'));
    const { rerender } = renderHook(({ hasAttachments }: { hasAttachments: boolean }) => usePipelineAttachmentYamlSync(hasAttachments), {
      initialProps: { hasAttachments: false },
    });

    expect(usePipelineYamlStore.getState().yamlJsonObject).toEqual({ state: {} });

    rerender({ hasAttachments: true });
    expect(usePipelineYamlStore.getState().yamlJsonObject).toMatchObject({
      state: { input_attachments: { type: 'list', value: [] } },
    });
  });
  it('adds/removes attachments atomically while retaining quoted numeric order and opaque raw declarations', () => {
    const source =
      '# source\r\nstate:\r\n  "10": list\r\n  "2": {type: dict, value: null, opaque: {keep: true}}\r\n  missing: {type: str}\r\nnodes: []\r\n';
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument(source));
    const updates: { code: string; document: Readonly<Record<string, unknown>>; order: readonly string[] }[] = [];
    const original = load(source) as { state: Record<string, unknown> };
    const atomicEdit = usePipelineYamlStore.getState().editPipelineYamlDocument;
    restoreAtomicEdit = () => usePipelineYamlStore.setState({editPipelineYamlDocument:atomicEdit});
    usePipelineYamlStore.setState({
      editPipelineYamlDocument: (document, options) => {
        const before = usePipelineYamlStore.getState();
        atomicEdit(document, options);
        const after = usePipelineYamlStore.getState();
        if (after !== before)
          updates.push({ code: after.yamlCode, document: after.yamlJsonObject, order: after.stateKeyOrder });
      },
    });
    const { rerender, unmount } = renderHook(
      ({ enabled }: { enabled: boolean }) => usePipelineAttachmentYamlSync(enabled),
      { initialProps: { enabled: false } },
    );
    const before = usePipelineYamlStore.getState();
    expect(before.yamlCode).toBe(source);
    rerender({ enabled: true });
    const added = usePipelineYamlStore.getState();

    expect(load(added.yamlCode)).toEqual(added.yamlJsonObject);
    expect(added.stateKeyOrder).toEqual(['10', '2', 'missing', 'input_attachments']);
    expect(parsePipelineYamlDocument(added.yamlCode).stateKeyOrder).toEqual(added.stateKeyOrder);
    expect((added.yamlJsonObject['state'] as Record<string, unknown>)['input_attachments']).toEqual({
      type: 'list',
      value: [],
    });
    rerender({ enabled: false });
    const removed = usePipelineYamlStore.getState();
    expect(removed.stateKeyOrder).toEqual(['10', '2', 'missing']);
    expect(load(removed.yamlCode)).toEqual({ ...original, nodes: [] });
    expect(updates).toHaveLength(2);
    expect(
      updates.every(
        ({ code, document, order }) =>
          parsePipelineYamlDocument(code).stateKeyOrder.join('\0') === order.join('\0') &&
          JSON.stringify(load(code)) === JSON.stringify(document),
      ),
    ).toBe(true);
    unmount();
    usePipelineYamlStore.setState({ editPipelineYamlDocument: atomicEdit });
  });

  it('uses current raw YAML when the flow view is stale instead of replacing a valid unsaved draft', () => {
    const initial = 'state: {"10": list, "2": dict}\nnodes: []\n';
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument(initial));
    const edited = 'state: {"10": list, "2": dict}\nnodes: [{id: unsaved, type: printer, template: current}]\n';
    usePipelineYamlStore.getState().setYamlCode(edited);
    renderHook(() => usePipelineAttachmentYamlSync(true));
    expect(usePipelineYamlStore.getState().yamlJsonObject['nodes']).toEqual([
      { id: 'unsaved', type: 'printer', template: 'current' },
    ]);
    expect(usePipelineYamlStore.getState().stateKeyOrder).toEqual(['10', '2', 'input_attachments']);
  });

  it('keeps an authored attachment descriptor and original bytes intact when already synchronized', () => {
    const source =
      '# authored\r\nstate: {"10": list, "2": dict, input_attachments: {type: list, default: [], opaque: true}}\r\nnodes: []\r\n';
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument(source));
    const before = usePipelineYamlStore.getState();
    renderHook(() => usePipelineAttachmentYamlSync(true));
    expect(usePipelineYamlStore.getState()).toBe(before);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(source);
  });

  it.each(['state: [', 'state: malformed', 'state: null', 'state: []'])(
    'does not overwrite invalid raw YAML %s',
    (source) => {
      usePipelineYamlStore.getState().initPipelineYaml({ yamlCode: source, yamlJsonObject: {} });
      const before = usePipelineYamlStore.getState();
      renderHook(() => usePipelineAttachmentYamlSync(true));
      expect(usePipelineYamlStore.getState()).toBe(before);
    },
  );

  it('does not publish a no-op source-based parse over the last valid flow view', () => {
    const original = 'state: {"10": list, "2": dict}\nnodes: []\n';
    usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument(original));
    usePipelineYamlStore.getState().setYamlCode(original.replace('nodes: []', 'nodes: [{id: draft, type: printer}]'));
    const before = usePipelineYamlStore.getState();
    renderHook(() => usePipelineAttachmentYamlSync(false));
    expect(usePipelineYamlStore.getState()).toBe(before);
  });

  it('generates only fields accepted by the actual Rust state descriptor source contract', () => {
    const sourceRoot = process.env['PIPELINE_RUST_SOURCE_ROOT'] ?? resolve(process.cwd(), '../..');
    const compiler = readFileSync(
      resolve(sourceRoot, 'services/elitea-worker-rust/src/agents/graph/compiler.rs'),
      'utf8',
    );
    const descriptor = compiler.match(/struct RawStateTypeDescriptor \{([\s\S]*?)\n\}/)?.[1];
    expect(descriptor).toBeDefined();
    expect(descriptor).toContain('#[serde(rename = "type")]');
    expect(descriptor).toMatch(/value:\s*Option<serde_json::Value>/);
    expect(descriptor).not.toMatch(/\bdefault:\s/);
    expect(compiler).toContain('"list" => value.is_array()');
    usePipelineYamlStore
      .getState()
      .initPipelineYaml(parsePipelineYamlDocument('state: {"10": list, "2": dict}\nnodes: []'));
    renderHook(() => usePipelineAttachmentYamlSync(true));
    const parsed = load(usePipelineYamlStore.getState().yamlCode) as { state: Record<string, Record<string, unknown>> };
    const generated = parsed.state['input_attachments']!;
    expect(Object.keys(generated).sort()).toEqual(['type', 'value']);
    expect(generated['type']).toBe('list');
    expect(Array.isArray(generated['value'])).toBe(true);
  });
});
