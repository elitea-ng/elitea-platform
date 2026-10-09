import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import type { GraphAdmissionIssue } from './graphAdmission.types';
import { extensionRecord, extensionRows, extensionText } from './graphExtensions.helpers';
import { AGGREGATE_OPERATIONS, SHAPING_LIMITS, shapingLimitKeys, type ExtensionRecord } from './graphExtensions.types';
import {
  extensionValidator, validateDeclaredExtensionChannel, validateDistinctExtensionNames, validateExtensionBoolean,
  validateExtensionEnum, validateExtensionInteger, validateExtensionList, validateExtensionObject,
  validateExtensionPointer, validateShapingName, type ExtensionValidator,
} from './graphExtensionValidation.helpers';

const COMMON = ['id', 'type', 'source', 'output', 'limits', 'transition'];
function validateCommon(document: YamlPipelineDocument, node: YamlPipelineNode, sourceType: string, check: ExtensionValidator): void {
  validateDeclaredExtensionChannel(document, node['source'], 'source', [sourceType], check);
  const outputs = validateExtensionList(node.output, 'output', 1, check);
  if (outputs.length !== 1) check.issue('output', 'Select exactly one output list state variable.');
  validateDeclaredExtensionChannel(document, outputs[0], 'output[0]', ['list'], check);
  if (outputs[0] === node['source']) check.issue('output[0]', 'Source and output must be different state variables.');
  if (node.transition !== undefined && node.transition !== null && typeof node.transition !== 'string') {
    check.issue('transition', 'Declare a node id, END, or null.');
  }
  if (node['limits'] !== undefined) {
    const keys = shapingLimitKeys(node.type);
    const limits = validateExtensionObject(node['limits'], 'limits', keys, [], check);
    for (const key of keys) {
      if (Object.hasOwn(limits, key)) validateExtensionInteger(limits[key], `limits.${key}`, SHAPING_LIMITS[key as keyof typeof SHAPING_LIMITS], check);
    }
  }
}
function validateSelection(value: unknown, field: string, missing: readonly string[], check: ExtensionValidator): ExtensionRecord {
  const selection = validateExtensionObject(value, field, ['path', 'output', 'missing'], ['path', 'output'], check);
  validateExtensionPointer(selection['path'], `${field}.path`, check);
  validateShapingName(selection['output'], `${field}.output`, check);
  if (selection['missing'] !== undefined) validateExtensionEnum(selection['missing'], `${field}.missing`, missing, check);
  return selection;
}
function validateRetention(value: unknown, field: string, check: ExtensionValidator, modes: readonly string[] = ['none', 'all', 'only', 'except']): void {
  const record = extensionRecord(value);
  const mode = extensionText(record['mode']);
  validateExtensionObject(value, field, mode === 'only' || mode === 'except' ? ['mode', 'fields'] : ['mode'], ['mode'], check);
  validateExtensionEnum(mode, `${field}.mode`, modes, check);
  if (mode === 'only') {
    const selections = validateExtensionList(record['fields'], `${field}.fields`, 0, check)
      .map((selection, index) => validateSelection(selection, `${field}.fields[${String(index)}]`, ['error', 'null', 'skip'], check));
    validateDistinctExtensionNames(selections.map((selection) => selection['output']), `${field}.fields`, check);
  }
  if (mode === 'except') {
    const fields = validateExtensionList(record['fields'], `${field}.fields`, 0, check);
    fields.forEach((name, index) => validateShapingName(name, `${field}.fields[${String(index)}]`, check));
    validateDistinctExtensionNames(fields, `${field}.fields`, check);
  }
}
function splitPolicyIssues(node: YamlPipelineNode, check: ExtensionValidator): void {
  for (const field of ['missing_list', 'null_list']) {
    if (node[field] !== undefined) validateExtensionEnum(node[field], field, ['error', 'empty'], check);
  }
  if (node['remove_source'] !== undefined) validateExtensionBoolean(node['remove_source'], 'remove_source', check);
  const retain = extensionRecord(node['retain']);
  if (retain['mode'] === 'only' && extensionRows(retain['fields']).some((row) => row['output'] === node['destination'])) {
    check.issue('destination', 'The item name collides with a retained field name.');
  }
}
function splitOutIssues(document: YamlPipelineDocument, node: YamlPipelineNode): readonly GraphAdmissionIssue[] {
  const check = extensionValidator(node.id, 'split_out.rs::SplitOutNodeDefinition::from_yaml');
  validateExtensionObject(node, '', [...COMMON, 'split', 'destination', 'retain', 'missing_list', 'null_list', 'remove_source'], ['source', 'output', 'split', 'destination'], check);
  const split = extensionRecord(node['split']);
  const mode = extensionText(split['mode']);
  validateExtensionObject(node['split'], 'split', mode === 'list' ? ['mode'] : ['mode', 'path'], ['mode'], check);
  validateExtensionEnum(mode, 'split.mode', ['list', 'row_field', 'rows_field'], check);
  if (mode !== 'list') validateExtensionPointer(split['path'], 'split.path', check);
  validateCommon(document, node, mode === 'row_field' ? 'dict' : 'list', check);
  validateShapingName(node['destination'], 'destination', check);
  if (mode === 'list' && node['retain'] !== undefined && extensionRecord(node['retain'])['mode'] !== 'none') {
    check.issue('retain.mode', 'Direct list mode has no parent fields to retain. Select none.');
  }
  if (node['retain'] !== undefined) validateRetention(node['retain'], 'retain', check);
  splitPolicyIssues(node, check);
  return check.issues;
}
function selectedFieldIssues(value: unknown, parent: string, operation: string, check: ExtensionValidator): void {
  const field = `${parent}.field`;
  const integer = operation.endsWith('_int');
  const selected = validateExtensionObject(value, field, ['path', 'missing', 'null'], ['path'], check);
  validateExtensionPointer(selected['path'], `${field}.path`, check);
  if (selected['missing'] !== undefined) validateExtensionEnum(selected['missing'], `${field}.missing`, ['error', 'null', 'skip'], check);
  if (selected['null'] === 'keep' && integer) check.issue(`${field}.null`, 'Integer operations cannot keep null. Select error or skip.');
  else if (selected['null'] !== undefined) validateExtensionEnum(selected['null'], `${field}.null`, integer ? ['error', 'skip'] : ['keep', 'error', 'skip'], check);
}
function operationIssues(value: unknown, index: number, check: ExtensionValidator): void {
  const field = `operations[${String(index)}]`;
  const record = extensionRecord(value);
  const operation = extensionText(record['operation']);
  const extra = operation === 'collect_rows' ? ['retain'] : operation === 'count_rows' ? [] : ['field'];
  if (operation === 'collect') extra.push('merge_lists');
  validateExtensionObject(value, field, ['operation', 'output', ...extra], ['operation', 'output'], check);
  validateExtensionEnum(operation, `${field}.operation`, AGGREGATE_OPERATIONS, check);
  validateShapingName(record['output'], `${field}.output`, check);
  if (operation === 'collect_rows' && record['retain'] !== undefined) validateRetention(record['retain'], `${field}.retain`, check, ['all', 'only', 'except']);
  if (operation === 'collect' && record['merge_lists'] !== undefined) validateExtensionBoolean(record['merge_lists'], `${field}.merge_lists`, check);
  if (operation !== 'count_rows' && operation !== 'collect_rows') {
    selectedFieldIssues(record['field'], field, operation, check);
  }
}
function aggregateIssues(document: YamlPipelineDocument, node: YamlPipelineNode): readonly GraphAdmissionIssue[] {
  const check = extensionValidator(node.id, 'aggregate.rs::AggregateNodeDefinition::from_yaml');
  validateExtensionObject(node, '', [...COMMON, 'layout', 'regroup', 'group_by', 'operations'], ['source', 'output', 'operations'], check);
  validateCommon(document, node, 'list', check);
  if (node['layout'] !== undefined) validateExtensionEnum(node['layout'], 'layout', ['plain', 'split_out'], check);
  if (node['group_by'] !== undefined) {
    const groups = validateExtensionList(node['group_by'], 'group_by', 0, check)
      .map((selection, index) => validateSelection(selection, `group_by[${String(index)}]`, ['error', 'null'], check));
    validateDistinctExtensionNames(groups.map((selection) => selection['output']), 'group_by', check);
  }
  if (node['regroup'] !== undefined) validateExtensionEnum(node['regroup'], 'regroup', ['none', 'parent'], check);
  const operations = validateExtensionList(node['operations'], 'operations', 1, check);
  operations.forEach((operation, index) => operationIssues(operation, index, check));
  const outputs = operations.map((operation) => extensionRecord(operation)['output']);
  validateDistinctExtensionNames(outputs, 'operations', check);
  const grouped = extensionRows(node['group_by']).map((group) => group['output']);
  outputs.forEach((output, index) => {
    if (grouped.includes(output)) check.issue(`operations[${String(index)}]`, 'This name already appears in group_by. Output names are unique across both lists.');
  });
  if (node['regroup'] === 'parent') {
    if (node['layout'] !== 'split_out') check.issue('regroup', 'Parent regrouping requires the split_out input row layout.');
    if (grouped.length > 0) check.issue('group_by', 'Parent regrouping cannot be combined with group fields.');
    operations.forEach((operation, index) => {
      if (extensionRecord(operation)['operation'] === 'collect_rows') check.issue(`operations[${String(index)}].operation`, 'Parent regrouping cannot use collect_rows.');
    });
  }
  return check.issues;
}
export function graphShapingIssues(document: YamlPipelineDocument): readonly GraphAdmissionIssue[] {
  return (document.nodes ?? []).flatMap((node) => {
    if (node.type === 'split_out') return splitOutIssues(document, node);
    if (node.type === 'aggregate') return aggregateIssues(document, node);
    return [];
  });
}
