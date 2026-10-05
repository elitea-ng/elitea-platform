import type { ReactNode } from 'react';
import { useContext } from 'react';
import { FlowEditorContext } from '../lib/flow-editor/flowEditorContext';
import { patchGraphExtensionNode } from '../lib/graphExtensions.helpers';
import { ParallelSettings } from '../ui/settings/parallel/ParallelSettings';

export const parallelYaml = `# Preserve state order and source modes.
entry_point: extension
state:
  '10': {type: list, value: [null, {vendor: [true, {deep: x}]}]}
  '2': {type: list, value: []}
  note: {type: str, value: null, vendor: {opaque: yes}}
  prefix: str
nodes:
  - id: extension
    type: parallel
    branches: [{id: stable_left, node: Agent_left}, {id: stable_right, node: Agent_right}]
    max_concurrency: 2
    wait: all
    output: ['10']
    transition: END
  - id: Agent_left
    type: agent
    tool: left_participant
    input_mapping: {task: {type: variable, value: prefix}, note: {type: fixed, value: ''}}
    output: [note]
  - id: Agent_right
    type: agent
    tool: right_participant
    input_mapping: {task: {type: fstring, value: 'Keep {prefix}.'}}
    output: [note]
`;

/** Exercise editable settings without changing the production authoring gate. */
export function EditableParallelSettings(): ReactNode {
  const context = useContext(FlowEditorContext);
  const node = context?.yamlJsonObject.nodes?.find((entry) => entry.id === 'extension');
  if (!context || !node) return null;
  return <ParallelSettings node={node} disabled={Boolean(context.disabled)}
    change={(field, value) => context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, node.id, field, value))}
    remove={(field) => context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, node.id, field, undefined, true))} />;
}
