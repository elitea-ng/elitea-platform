import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';
import type { ExtensionRecord } from './graphExtensions.types';

export function extensionRecord(value: unknown): ExtensionRecord {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return {};
  return value as ExtensionRecord;
}
export function extensionText(value: unknown): string {
  return typeof value === 'string' ? value : '';
}
export function extensionNumberText(value: unknown): string {
  return typeof value === 'number' ? String(value) : extensionText(value);
}
export function extensionValues(value: unknown): readonly unknown[] {
  return Array.isArray(value) ? value as readonly unknown[] : [];
}
export function extensionRows(value: unknown): readonly ExtensionRecord[] {
  return Array.isArray(value) ? value.map((entry: unknown) => extensionRecord(entry)) : [];
}
export function extensionStrings(value: unknown): readonly string[] {
  return Array.isArray(value) ? value.filter((entry: unknown): entry is string => typeof entry === 'string') : [];
}
/** Remove a field only after an explicit control action. Keep every other authored value. */
export function patchExtensionRecord(record: ExtensionRecord, field: string, value: unknown, remove = false): ExtensionRecord {
  const next = { ...record };
  if (remove) delete next[field];
  else next[field] = value;
  return next;
}
export function patchGraphExtensionNode(document: YamlPipelineDocument, id: string, field: string, value: unknown, remove = false): YamlPipelineDocument {
  return { ...document, nodes: document.nodes?.map((node) => node.id === id
    ? { ...patchExtensionRecord(node, field, value, remove), id: node.id } : node) ?? [] };
}
export function declaredChannelType(value: unknown): string {
  return typeof value === 'string' ? value : extensionText(extensionRecord(value)['type']);
}
/** Sequence metadata owns existing declaration order. Object keys only append new declarations. */
export function orderedDeclaredChannels(document: YamlPipelineDocument, order: readonly string[]): readonly string[] {
  const state = document.state ?? {};
  const kept = order.filter((key, index) => Object.hasOwn(state, key) && order.indexOf(key) === index);
  return [...kept, ...Object.keys(state).filter((key) => !kept.includes(key))];
}
/** Show only authored defaults. A value never declares a nested schema. */
function boundedPreviewValue(value: unknown, depth: number, budget: { remaining: number }, seen: Set<object>): unknown {
  if (--budget.remaining < 0 || depth === 0) return '…';
  if (typeof value === 'string') return value.length > 80 ? `${value.slice(0, 80)}…` : value;
  if (value === null || typeof value !== 'object') return value;
  if (seen.has(value)) return '…';
  seen.add(value);
  if (Array.isArray(value)) return value.slice(0, 8).map((entry: unknown) => boundedPreviewValue(entry, depth - 1, budget, seen));
  const record = extensionRecord(value);
  return Object.fromEntries(Object.keys(record).slice(0, 8).map((key) => [key, boundedPreviewValue(record[key], depth - 1, budget, seen)]));
}
export function channelDefaultPreview(spec: unknown): string {
  const descriptor = extensionRecord(spec);
  if (!Object.hasOwn(descriptor, 'value')) return '';
  const encoded = JSON.stringify(boundedPreviewValue(descriptor['value'], 4, { remaining: 32 }, new Set()));
  if (encoded === undefined) return '';
  return encoded.length > 160 ? `${encoded.slice(0, 160)}…` : encoded;
}
export function jsonPointerTokens(value: unknown): readonly string[] | undefined {
  if (typeof value !== 'string' || !value.startsWith('/') || new TextEncoder().encode(value).length > 512) return undefined;
  const parts = value.slice(1).split('/');
  if (parts.length > 32 || parts.some((part) => /~(?:[^01]|$)/u.test(part))) return undefined;
  return parts.map((part) => part.replaceAll('~1', '/').replaceAll('~0', '~'));
}
/** Select a preview from an authored default without changing the document. */
export function pointerDefaultPreview(spec: unknown, path: unknown): string {
  const parts = jsonPointerTokens(path);
  const descriptor = extensionRecord(spec);
  if (!parts || !Object.hasOwn(descriptor, 'value')) return '';
  let value: unknown = descriptor['value'];
  for (const part of parts) {
    if (Array.isArray(value)) {
      if (!/^(0|[1-9][0-9]*)$/u.test(part)) return '';
      value = value[Number(part)];
    } else {
      const record = extensionRecord(value);
      if (!Object.hasOwn(record, part)) return '';
      value = record[part];
    }
  }
  return channelDefaultPreview({ value });
}
