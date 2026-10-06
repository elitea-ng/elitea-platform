import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { routeTargetsOf } from './graphAdmission.nodeReads';
import type { GraphAdmissionIssue } from './graphAdmission.types';
import { fixedParallelWorkerReferences } from './graphExtensionReferenceRename.helpers';
import { extensionStrings } from './graphExtensions.helpers';
import {
  extensionValidator, validExtensionChannel, validateDeclaredExtensionChannel, validateDistinctExtensionNames,
  validateExtensionEnum, validateExtensionInteger, validateExtensionList, validateExtensionObject, type ExtensionValidator,
} from './graphExtensionValidation.helpers';

/** The exact worker scope owns these channels. No state descriptors are created. */
export function mapOwnedWorkerChannels(document: YamlPipelineDocument, workerId: string): readonly string[] {
  const owners = (document.nodes ?? []).filter((node) => node.type === 'map' && node['worker'] === workerId);
  if (owners.length !== 1) return [];
  const owner = owners[0]!;
  const worker = document.nodes?.find((node) => node.id === workerId);
  if (!worker || (worker.type !== 'state_modifier' && worker.type !== 'agent')) return [];
  const channels = [owner['item'], owner['index']];
  if (!channels.every((value) => validExtensionChannel(value) && !Object.hasOwn(document.state ?? {}, value))
    || channels[0] === channels[1] || owner.id === workerId
    || (document.nodes ?? []).some((node) => node.type === 'parallel' && fixedParallelWorkerReferences(node['branches']).includes(workerId))) return [];
  return channels as readonly string[];
}
function mapLocalChannels(document: YamlPipelineDocument, node: YamlPipelineNode, check: ExtensionValidator): void {
  for (const field of ['item', 'index']) {
    const key = node[field];
    if (!validExtensionChannel(key) || Object.hasOwn(document.state ?? {}, key)) {
      check.issue(field, 'Use a child-local channel name that does not collide with parent or runtime state.');
    }
  }
  if (node['item'] === node['index']) check.issue('index', 'Item and index channels must be different.');
}
function mapWorkerTopology(document: YamlPipelineDocument, worker: YamlPipelineNode, check: ExtensionValidator): void {
  const owners = (document.nodes ?? []).filter((entry) => entry.type === 'map' && entry['worker'] === worker.id);
  const parallelOwner = (document.nodes ?? []).some((entry) => entry.type === 'parallel' && fixedParallelWorkerReferences(entry['branches']).includes(worker.id));
  if (owners.length !== 1 || parallelOwner) check.issue('worker', 'The worker must belong to exactly one Map.');
  if (typeof worker.transition === 'string') check.issue('worker', 'Remove the owned worker transition in its configuration.');
  if (document.entry_point === worker.id || extensionStrings(document.interrupt_before).includes(worker.id)
    || extensionStrings(document.interrupt_after).includes(worker.id)) check.issue('worker', 'The owned worker cannot be an entry point or static pause.');
  const routed = (document.nodes ?? []).some((entry) => routeTargetsOf({ id: entry.id, type: entry.type ?? '', raw: entry })
    .some((route) => route.target === worker.id));
  if (routed) check.issue('worker', 'A parent route cannot target the owned worker.');
}
function mapWorkerBindings(document: YamlPipelineDocument, node: YamlPipelineNode, worker: YamlPipelineNode, check: ExtensionValidator): void {
  const inputs = [...mapOwnedWorkerChannels(document, worker.id), ...extensionStrings(node['broadcast'])];
  if (extensionStrings(worker.input).some((key) => !inputs.includes(key))) check.issue('worker.input', 'Worker inputs must use item, index, or explicitly broadcast state.');
  if (extensionStrings(worker['variables_to_clean']).some((key) => !extensionStrings(worker.output).includes(key))) {
    check.issue('worker.variables_to_clean', 'The worker can clean only its declared output channels.');
  }
  if (worker.type === 'agent') {
    for (const [key, mapping] of Object.entries(worker.input_mapping ?? {})) {
      if (mapping.type === 'variable' && !(typeof mapping.value === 'string' && inputs.includes(mapping.value))) {
        check.issue(`worker.input_mapping.${key}`, 'Variable mappings can read only item, index, or approved broadcasts.');
      }
    }
  }
  for (const key of extensionStrings(node['outputs'])) {
    if (!extensionStrings(worker.output).includes(key)) check.issue('outputs', `The owned worker does not declare output ${key}.`);
  }
}
function mapWorkerIssues(document: YamlPipelineDocument, node: YamlPipelineNode, check: ExtensionValidator): void {
  const worker = (document.nodes ?? []).find((entry) => entry.id === node['worker']);
  if (!worker || worker.id === node.id || (worker.type !== 'state_modifier' && worker.type !== 'agent')) {
    check.issue('worker', 'Select one declared StateModifier or a verified saved Agent.');
    return;
  }
  mapWorkerTopology(document, worker, check);
  mapWorkerBindings(document, node, worker, check);
}
function mapIssues(document: YamlPipelineDocument, node: YamlPipelineNode): readonly GraphAdmissionIssue[] {
  const check = extensionValidator(node.id, 'map_yaml.rs::MapNodeDefinition::from_yaml, map_compiler.rs::validate_map_ownership');
  validateExtensionObject(node, '', ['id', 'type', 'worker', 'source', 'item', 'index', 'outputs', 'destination', 'max_items', 'max_concurrency', 'reduction', 'broadcast', 'transition'],
    ['worker', 'source', 'item', 'index', 'outputs', 'destination', 'max_items', 'max_concurrency', 'reduction'], check);
  validateDeclaredExtensionChannel(document, node['source'], 'source', ['list'], check);
  validateDeclaredExtensionChannel(document, node['destination'], 'destination', ['list'], check);
  mapLocalChannels(document, node, check);
  const outputs = validateExtensionList(node['outputs'], 'outputs', 1, check);
  const broadcast = node['broadcast'] === undefined ? [] : validateExtensionList(node['broadcast'], 'broadcast', 0, check);
  for (const [field, values] of [['outputs', outputs], ['broadcast', broadcast]] as const) {
    values.forEach((value, index) => validateDeclaredExtensionChannel(document, value, `${field}[${String(index)}]`, undefined, check));
    validateDistinctExtensionNames(values, field, check);
  }
  validateExtensionInteger(node['max_items'], 'max_items', 64, check);
  validateExtensionInteger(node['max_concurrency'], 'max_concurrency', 8, check);
  validateExtensionEnum(node['reduction'], 'reduction', ['ordered_collection'], check);
  if (node.transition !== undefined && node.transition !== null && typeof node.transition !== 'string') {
    check.issue('transition', 'Declare a node id, END, or null.');
  }
  mapWorkerIssues(document, node, check);
  return check.issues;
}
export function graphMapIssues(document: YamlPipelineDocument): readonly GraphAdmissionIssue[] {
  return (document.nodes ?? []).filter((node) => node.type === 'map').flatMap((node) => mapIssues(document, node));
}
