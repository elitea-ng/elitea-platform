import { renderHook } from '@testing-library/react';
import { beforeEach, expect, it } from 'vitest';

import { parsePipelineYamlDocument } from '../lib/pipelineYamlDocument.helpers';
import { usePipelineYamlStore } from './pipelineYamlStore';
import { usePipelineEditorStore } from './pipelineEditorStore';
import { usePipelineGraphDraft } from './usePipelineGraphDraft';

beforeEach(() => {
  usePipelineYamlStore.getState().initPipelineYaml({ yamlCode: '', yamlJsonObject: {} });
  usePipelineEditorStore.setState({ nodes: [], edges: [] });
});

const source =
  '# keep original bytes\r\nstate:\r\n  "2": list\r\n  "10": {type: dict, value: null, opaque: {keep: true}}\r\n  absent: {type: str}\r\nentry_point: code\r\nnodes:\r\n  - id: code\r\n    type: code\r\n    language: python\r\n    code: |\r\n      text = f"{value}: \'quoted\'"\r\n      return {"text": text}\r\n    transition: END\r\n';

it('passes exact original instructions to the real save-draft reader after a no-op/layout edit', () => {
  usePipelineYamlStore.getState().initPipelineYaml(parsePipelineYamlDocument(source));
  const { result } = renderHook(() => usePipelineGraphDraft());
  usePipelineYamlStore.getState().editPipelineYamlDocument({ ...usePipelineYamlStore.getState().yamlJsonObject });
  usePipelineYamlStore.getState().setLayoutVersion('layout-only');
  expect(result.current()?.instructions).toBe(source);
  expect(result.current()?.pipelineSettings.layout_version).toBeDefined();
});

it('passes semantic edits with exact raw declarations/order and unchanged multiline Code source to the save-draft reader', () => {
  const original = parsePipelineYamlDocument(source);
  usePipelineYamlStore.getState().initPipelineYaml(original);
  const { result } = renderHook(() => usePipelineGraphDraft());
  const next = { ...original.yamlJsonObject, interrupt_before: ['code'] };
  usePipelineYamlStore.getState().editPipelineYamlDocument(next);
  const instructions = result.current()?.instructions;
  expect(instructions).toBeDefined();
  const saved = parsePipelineYamlDocument(instructions!);
  expect(saved.yamlJsonObject).toEqual(next);
  expect(saved.stateKeyOrder).toEqual(['2', '10', 'absent']);
  expect(saved.yamlJsonObject['nodes']).toEqual(original.yamlJsonObject['nodes']);
});
