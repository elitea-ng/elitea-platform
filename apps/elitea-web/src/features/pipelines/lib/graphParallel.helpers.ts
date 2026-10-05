import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { isValidGraphId } from './graphAdmission.nodeReads';
import { extensionRecord, extensionValues } from './graphExtensions.helpers';

/** Strict declared node references. Branch IDs are stable result keys, never child node aliases. */
export function parallelAgentReferenceCounts(document: YamlPipelineDocument): ReadonlyMap<string, number> {
  const counts = new Map<string, number>();
  for (const node of document.nodes ?? []) {
    if (node.type !== 'parallel') continue;
    for (const value of extensionValues(node['branches'])) {
      const id = extensionRecord(value)['node'];
      if (typeof id === 'string') counts.set(id, (counts.get(id) ?? 0) + 1);
    }
  }
  return counts;
}

function uniqueDeclaredNode(document: YamlPipelineDocument, id: string, type: string): YamlPipelineNode | undefined {
  const matches = (document.nodes ?? []).filter((node) => node.id === id);
  return matches.length === 1 && matches[0]?.type === type ? matches[0] : undefined;
}
function strictBranchReference(value: unknown, ownerId: string): string | undefined {
  const branch = extensionRecord(value);
  if (Object.keys(branch).length !== 2 || typeof branch['id'] !== 'string' || !isValidGraphId(branch['id'])
    || typeof branch['node'] !== 'string' || !isValidGraphId(branch['node']) || branch['node'] === ownerId) return undefined;
  return branch['node'];
}
/** Return an Agent only when this exact branch is its sole fixed owner. Do not repair invalid ownership. */
export function parallelOwnedAgent(document: YamlPipelineDocument, ownerId: string, ordinal: number): YamlPipelineNode | undefined {
  const owner = uniqueDeclaredNode(document, ownerId, 'parallel');
  if (!owner) return undefined;
  const reference = strictBranchReference(extensionValues(owner['branches'])[ordinal], ownerId);
  if (!reference || parallelAgentReferenceCounts(document).get(reference) !== 1
    || document.nodes?.some((node) => node.type === 'map' && node['worker'] === reference)) return undefined;
  return uniqueDeclaredNode(document, reference, 'agent');
}

/** Change one explicit field and retain unknown branch data for compiler refusal. */
export function patchParallelBranch(branches: unknown, ordinal: number, field: 'id' | 'node', value: string): readonly unknown[] {
  return extensionValues(branches).map((branch, index) => index === ordinal ? { ...extensionRecord(branch), [field]: value } : branch);
}

export function moveParallelBranch(branches: unknown, ordinal: number, offset: -1 | 1): readonly unknown[] {
  const values = [...extensionValues(branches)];
  const target = ordinal + offset;
  if (ordinal < 0 || ordinal >= values.length || target < 0 || target >= values.length) return values;
  [values[ordinal], values[target]] = [values[target], values[ordinal]];
  return values;
}

export function nextParallelBranchId(branches: unknown): string {
  const ids = new Set(extensionValues(branches).map((branch) => extensionRecord(branch)['id']));
  let ordinal = 1;
  while (ids.has(`branch_${String(ordinal)}`)) ordinal += 1;
  return `branch_${String(ordinal)}`;
}
