import type { ReactNode } from 'react';
import type { NodeProps } from '@xyflow/react';
import { ReactFlowProvider } from '@xyflow/react';
import { beforeAll } from 'vitest';
import type { FlowNode } from '../lib/flow-editor/reactFlowTypes';
import { FlowEditorContext } from '../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../lib/flow-editor/helpers/pipelineFlow.types';
import { parsePipelineYamlDocument } from '../lib/pipelineYamlDocument.helpers';
import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { buildFlowEditorContextValue, renderWithRouterAndProject } from './testUtils';

beforeAll(() => {
  globalThis.ResizeObserver ??= class {
    observe(): void {} unobserve(): void {} disconnect(): void {}
  };
});
const extensionStateYaml = `state:
  '10': {type: list, value: []}
  '2': dict
  source_rows: {type: list, value: [null, 1, {vendor: [true, {deep: x}]}]}
  source_object: {type: dict, value: {a/b: {orders: [null, {x: 2}]}}}
  item_result: {type: str, value: null, vendor: {opaque: yes}}
  prefix: str
`;
export const mapNodeYaml = `  - id: extension
    type: map
    worker: render_item
    source: source_rows
    item: entity
    index: item_index
    outputs: [item_result]
    destination: '10'
    max_items: 64
    max_concurrency: 2
    reduction: ordered_collection
    broadcast: [prefix]
    transition: END
  - id: render_item
    type: state_modifier
    input: [entity, item_index, prefix]
    output: [item_result]
    template: '{{ entity }}'
`;
export const splitNodeYaml = `  - id: extension
    type: split_out
    source: source_object
    split: {mode: row_field, path: /a~1b/orders}
    destination: item
    output: ['10']
    transition: END
`;
export const aggregateNodeYaml = `  - id: extension
    type: aggregate
    source: source_rows
    operations: [{operation: count_rows, output: count}]
    output: ['10']
    transition: END
`;
export function authoredExtensionYaml(nodeYaml: string): string {
  return `# Keep authored state order and all defaults.
entry_point: start
${extensionStateYaml}nodes:
  - id: start
    type: printer
    transition: extension
${nodeYaml}`;
}
export function currentExtensionDocument(): YamlPipelineDocument {
  return usePipelineYamlStore.getState().yamlJsonObject;
}
export function renderExtensionCard(Component: (props: NodeProps<FlowNode>) => ReactNode, yaml: string, disabled = false) {
  const parsed = parsePipelineYamlDocument(yaml);
  usePipelineYamlStore.getState().initPipelineYaml({ yamlCode: yaml, yamlJsonObject: parsed.yamlJsonObject });
  function BoundCard(): ReactNode {
    const document = usePipelineYamlStore((state) => state.yamlJsonObject) as YamlPipelineDocument;
    const edit = usePipelineYamlStore((state) => state.editPipelineYamlDocument);
    const value = buildFlowEditorContextValue({ yamlJsonObject: document, setYamlJsonObject: edit, expandAll: true, disabled });
    const node = document.nodes?.find((entry) => entry.id === 'extension');
    const props = { id: 'extension', type: node?.type, data: {}, selected: true } as unknown as NodeProps<FlowNode>;
    return <ReactFlowProvider><FlowEditorContext.Provider value={value}><Component {...props} /></FlowEditorContext.Provider></ReactFlowProvider>;
  }
  return renderWithRouterAndProject(<BoundCard />, undefined);
}
