import { RuntimeContractConstants } from './flow-editor/constants';
import { BUILTIN_STATE_KEYS } from './graphAdmission.nodeReads';
import { admissionIssue, type GraphAdmissionIssue } from './graphAdmission.types';
import { declaredChannelType, extensionRecord, extensionText, jsonPointerTokens } from './graphExtensions.helpers';
import type { ExtensionRecord } from './graphExtensions.types';
import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';

export interface ExtensionValidator {
  readonly issues: GraphAdmissionIssue[];
  readonly issue: (field: string, message: string) => void;
}
export function extensionValidator(id: string, citation: string): ExtensionValidator {
  const issues: GraphAdmissionIssue[] = [];
  return { issues, issue: (field, message) => {
    issues.push(admissionIssue('node.extension-configuration', citation, id, field, '', `${field}: ${message}`));
  } };
}
export function validateExtensionObject(value: unknown, field: string, allowed: readonly string[], required: readonly string[], check: ExtensionValidator): ExtensionRecord {
  const record = extensionRecord(value);
  if (value === null || typeof value !== 'object' || Array.isArray(value)) check.issue(field, 'Declare a mapping.');
  for (const key of Object.keys(record)) {
    if (!allowed.includes(key)) check.issue(field ? `${field}.${key}` : key, 'This field is not part of this node contract.');
  }
  for (const key of required) {
    if (!Object.hasOwn(record, key)) check.issue(field ? `${field}.${key}` : key, 'This field is required.');
  }
  return record;
}
export function validateExtensionEnum(value: unknown, field: string, allowed: readonly string[], check: ExtensionValidator): void {
  if (typeof value !== 'string' || !allowed.includes(value)) check.issue(field, `Select ${allowed.join(', ')}.`);
}
export function validateExtensionBoolean(value: unknown, field: string, check: ExtensionValidator): void {
  if (typeof value !== 'boolean') check.issue(field, 'Declare true or false.');
}
export function validateExtensionInteger(value: unknown, field: string, ceiling: number, check: ExtensionValidator): void {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 1 || value > ceiling) {
    check.issue(field, `Declare an integer from 1 to ${String(ceiling)}.`);
  }
}
export function validateExtensionPointer(value: unknown, field: string, check: ExtensionValidator): void {
  if (!jsonPointerTokens(value)) check.issue(field, 'Use a non-empty RFC 6901 pointer with at most 512 bytes and 32 segments.');
}
export function validExtensionChannel(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && new TextEncoder().encode(value).length <= 256
    && !value.includes('\u0000') && !value.includes('\r') && !value.includes('\n')
    && !RuntimeContractConstants.isReservedStateKey(value) && !BUILTIN_STATE_KEYS.has(value);
}
export function validateDeclaredExtensionChannel(document: YamlPipelineDocument, value: unknown, field: string, types: readonly string[] | undefined, check: ExtensionValidator): void {
  if (!validExtensionChannel(value) || !Object.hasOwn(document.state ?? {}, value)) {
    check.issue(field, 'Select a declared user state variable.');
    return;
  }
  if (types && !types.includes(declaredChannelType(document.state?.[value]))) {
    check.issue(field, `The state variable must declare ${types.join(' or ')}.`);
  }
}
export function validateShapingName(value: unknown, field: string, check: ExtensionValidator): void {
  const name = extensionText(value);
  if (name.length === 0 || new TextEncoder().encode(name).length > 256
    || /\p{Cc}/u.test(name)) {
    check.issue(field, 'Use a non-empty literal name with at most 256 bytes and no control characters.');
  }
}
export function validateExtensionList(value: unknown, field: string, minimum: number, check: ExtensionValidator): readonly unknown[] {
  if (!Array.isArray(value)) {
    check.issue(field, 'Declare a sequence.');
    return [];
  }
  if (value.length < minimum || value.length > 64) check.issue(field, `Declare ${String(minimum)} to 64 entries.`);
  return value as readonly unknown[];
}
export function validateDistinctExtensionNames(values: readonly unknown[], field: string, check: ExtensionValidator): void {
  const seen = new Set<unknown>();
  values.forEach((value, index) => {
    if (seen.has(value)) check.issue(`${field}[${String(index)}]`, 'This name already appears in this sequence.');
    seen.add(value);
  });
}
