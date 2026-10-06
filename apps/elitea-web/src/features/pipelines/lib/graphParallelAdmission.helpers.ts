import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { isValidGraphId, routeTargetsOf } from './graphAdmission.nodeReads';
import type { GraphAdmissionIssue } from './graphAdmission.types';
import { extensionStrings, extensionValues } from './graphExtensions.helpers';
import { parallelAgentReferenceCounts } from './graphParallel.helpers';
import {
  extensionValidator, validateDeclaredExtensionChannel, validateExtensionEnum, validateExtensionInteger,
  validateExtensionObject, type ExtensionValidator,
} from './graphExtensionValidation.helpers';

function validateOwnedAgentTopology(document: YamlPipelineDocument, agent: YamlPipelineNode, field: string, check: ExtensionValidator): void {
  if (agent.transition !== undefined && agent.transition !== null) check.issue(field, 'Remove the owned Agent transition, including END.');
  if (document.entry_point === agent.id || extensionStrings(document.interrupt_before).includes(agent.id)
    || extensionStrings(document.interrupt_after).includes(agent.id)) check.issue(field, 'The owned Agent cannot be an entry point or static pause.');
  if (document.nodes?.some((node) => routeTargetsOf({ id: node.id, type: node.type ?? '', raw: node }).some((route) => route.target === agent.id))) {
    check.issue(field, 'A parent route cannot target the owned Agent.');
  }
}
/** yaml.rs::RawParallelBranchDefinition and parallel_compiler.rs::validate_parallel_ownership. */
function validateOwnedAgent(document: YamlPipelineDocument, owner: YamlPipelineNode, value: unknown, field: string, check: ExtensionValidator): void {
  if (typeof value !== 'string' || !isValidGraphId(value) || value === owner.id) {
    check.issue(field, 'Select a different declared Agent node id.');
    return;
  }
  const matches = (document.nodes ?? []).filter((node) => node.id === value);
  const agent = matches[0];
  if (matches.length !== 1 || agent?.type !== 'agent') {
    check.issue(field, 'Select one exact declared Agent node id.');
    return;
  }
  if (parallelAgentReferenceCounts(document).get(value) !== 1
    || document.nodes?.some((node) => node.type === 'map' && node['worker'] === value)) {
    check.issue(field, 'The Agent must belong to exactly one fixed Parallel branch.');
  }
  validateOwnedAgentTopology(document, agent, field, check);
}

function parallelIssues(document: YamlPipelineDocument, node: YamlPipelineNode): readonly GraphAdmissionIssue[] {
  const check = extensionValidator(node.id, 'yaml.rs::ParallelNodeDefinition::from_yaml, parallel_compiler.rs::validate_parallel_ownership, compiler.rs::validate_node_state');
  validateExtensionObject(node, '', ['id', 'type', 'branches', 'max_concurrency', 'wait', 'error_policy', 'output', 'transition'],
    ['branches', 'max_concurrency', 'wait', 'output'], check);
  const branches = extensionValues(node['branches']);
  if (!Array.isArray(node['branches']) || branches.length < 2 || branches.length > 16) check.issue('branches', 'Declare 2 to 16 fixed branches.');
  const ids = new Set<string>();
  branches.forEach((value, ordinal) => {
    const field = `branches[${String(ordinal)}]`;
    const branch = validateExtensionObject(value, field, ['id', 'node'], ['id', 'node'], check);
    const id = branch['id'];
    if (typeof id !== 'string' || !isValidGraphId(id)) check.issue(`${field}.id`, 'Use a stable ASCII result key with at most 128 bytes.');
    else if (ids.has(id)) check.issue(`${field}.id`, 'Each branch result key must be unique.');
    else ids.add(id);
    validateOwnedAgent(document, node, branch['node'], `${field}.node`, check);
  });
  validateExtensionInteger(node['max_concurrency'], 'max_concurrency', 8, check);
  if (typeof node['max_concurrency'] === 'number' && node['max_concurrency'] > branches.length) {
    check.issue('max_concurrency', 'Concurrency cannot exceed the declared branch count.');
  }
  validateExtensionEnum(node['wait'], 'wait', ['all'], check);
  if (Object.hasOwn(node, 'error_policy')) validateExtensionEnum(node['error_policy'], 'error_policy', ['fail_after_drain'], check);
  if (!Array.isArray(node.output) || node.output.length !== 1) check.issue('output', 'Declare exactly one ordered list output channel.');
  else validateDeclaredExtensionChannel(document, node.output[0], 'output[0]', ['list'], check);
  if (node.transition !== undefined && node.transition !== null && typeof node.transition !== 'string') {
    check.issue('transition', 'Declare a node id, END, or null.');
  }
  return check.issues;
}

export function graphParallelIssues(document: YamlPipelineDocument): readonly GraphAdmissionIssue[] {
  return (document.nodes ?? []).filter((node) => node.type === 'parallel').flatMap((node) => parallelIssues(document, node));
}
