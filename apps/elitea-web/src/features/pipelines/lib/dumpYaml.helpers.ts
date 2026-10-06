/**
 * Local duplicate of `apps/elitea-ui/src/[fsd]/shared/lib/helpers/
 * dumpYaml.helpers.js` (`dumpYaml`) — the pipeline-flow-editor's custom
 * YAML serializer (node `id`/`type` reordered first, top-level keys ordered
 * `state -> entry_point -> interrupt_after -> interrupt_before -> nodes`,
 * `lineWidth: -1` to prevent wrapping).
 *
 * This lives under `shared/lib/` in the baseline (unit S3's ownership
 * fence), but is NOT present anywhere in this worktree as of this sub-unit's
 * (A2n) writing — verified: `grep -rl "reorderNodeKeys\|lineWidth: -1" src`
 * returns zero hits, and S3 (marked complete) evidently did not carry this
 * specific file across. Two of A2n's own owned files need it directly
 * (`EditorPanel.tsx`'s `setYamlJsonObject`, mirroring the baseline's
 * `EditorPanel.jsx:90`; `useIsPipelineYamlCodeDirty.ts`'s re-dump comparison,
 * mirroring `useIsPipelineYamlCodeDirty.js:24`), so — per this mission's own
 * established precedent for a genuinely-needed, not-owned, not-yet-landed
 * dependency (see the preamble's four-hooks list and this same worktree's
 * `features/agents/lib/hooks/applicationChat.helpers.ts`'s
 * `getWelcomeMessage`/`getInitialChatHistory` duplication for the identical
 * situation) — it is duplicated locally here rather than invented,
 * skipped, or imported from a path that does not exist. A future `shared/
 * lib` pass can promote this verbatim and both call sites can switch to the
 * promoted import with zero behavioural change.
 *
 * **Disclosed deviation:** the baseline's `dump(processedData, { lineWidth:
 * -1, sortKeys, noCompatMode: true })` passes a `noCompatMode` flag that
 * does not exist on this app's pinned `js-yaml@5.2.2`'s `DumpOptions` type
 * (verified: `node_modules/js-yaml/dist/js-yaml.d.ts`'s `PresenterOptions`/
 * `DumpOptions` interfaces list no such field — that flag was removed
 * between the baseline's js-yaml v3 and this app's v5). Omitted here rather
 * than passed as an unchecked excess property; v5's dumper already emits
 * the modern (non-"compat") style unconditionally, so this is a type-only
 * no-op, not a behaviour change.
 */
import { dump, load } from 'js-yaml';
import type { Document, MappingNode, Node } from 'js-yaml';

import {
  pipelineValueFingerprint,
  readPipelineStateOrder,
  reconcilePipelineStateOrder,
  validatePipelineStateOrder,
} from './pipelineYamlState.helpers';

/** `state -> entry_point -> interrupt_after -> interrupt_before -> nodes` — baseline `dumpYaml.helpers.js:4`. */
const TOP_LEVEL_KEY_ORDER: readonly string[] = ['state', 'entry_point', 'interrupt_after', 'interrupt_before', 'nodes'];

/** Node-object field priority — `id`/`type` always sort first among a node's own keys. */
const NODE_PRIORITY_FIELDS: readonly string[] = ['id', 'type'];

function compareByOrder(a: string, b: string, orderArray: readonly string[]): number {
  const indexA = orderArray.indexOf(a);
  const indexB = orderArray.indexOf(b);
  if (indexA !== -1 && indexB !== -1) return indexA - indexB;
  if (indexA !== -1) return -1;
  if (indexB !== -1) return 1;
  return a.localeCompare(b);
}

function isSerializable(value: unknown): boolean {
  const type = typeof value;
  return type !== 'function' && type !== 'symbol';
}

function reorderNodeKeys(obj: unknown): unknown {
  if (!obj || typeof obj !== 'object') return obj;

  if (obj instanceof Date || obj instanceof Uint8Array) return obj;

  if (Array.isArray(obj)) {
    return obj.map((item) => reorderNodeKeys(item));
  }

  const record = obj as Record<string, unknown>;

  return Object.fromEntries(
    Object.entries(record)
      .filter(([, value]) => isSerializable(value))
      .map(([key, value]) => [key, reorderNodeKeys(value)]),
  );
}

export interface DumpYamlOptions {
  readonly originalYaml?: string;
  readonly stateKeyOrder?: readonly string[];
}

function nodeKey(node: Node): string {
  return node.kind === 'scalar' ? node.value : '';
}

function sortMapping(mapping: MappingNode, order: readonly string[]): void {
  mapping.items.sort((a, b) => compareByOrder(nodeKey(a.key), nodeKey(b.key), order));
}

function orderRootMapping(root: Node | null, stateOrder: readonly string[]): void {
  if (root?.kind !== 'mapping') return;
  sortMapping(root, TOP_LEVEL_KEY_ORDER);
  const state = root.items.find(({ key }) => nodeKey(key) === 'state')?.value;
  if (state?.kind === 'mapping') sortMapping(state, stateOrder);
}

function orderNodes(root: Node | null): void {
  const nodes = root?.kind === 'mapping' ? root.items.find(({ key }) => nodeKey(key) === 'nodes')?.value : root;
  if (nodes?.kind !== 'sequence') return;
  for (const node of nodes.items) {
    if (node.kind === 'mapping') sortMapping(node, NODE_PRIORITY_FIELDS);
  }
}

function orderYamlMappings(documents: Document[], stateOrder: readonly string[]): void {
  for (const document of documents) {
    orderRootMapping(document.contents, stateOrder);
    orderNodes(document.contents);
  }
}

/** Serialize an instruction edit. Keep original text when its contract is unchanged. */
export function serializePipelineYaml(data: unknown, options: DumpYamlOptions = {}): string {
  const originalOrder = options.originalYaml === undefined ? [] : readPipelineStateOrder(options.originalYaml);
  const stateOrder = options.stateKeyOrder ?? reconcilePipelineStateOrder(data, originalOrder);
  validatePipelineStateOrder(data, stateOrder);
  const fingerprint = pipelineValueFingerprint(data, stateOrder);
  if (options.originalYaml !== undefined) {
    const original = load(options.originalYaml || '');
    if (fingerprint === pipelineValueFingerprint(original, originalOrder)) return options.originalYaml;
  }
  const result = dump(reorderNodeKeys(data), {
    lineWidth: -1,
    transform: (documents) => orderYamlMappings(documents, stateOrder),
  });
  if (fingerprint !== pipelineValueFingerprint(load(result), readPipelineStateOrder(result))) {
    throw new Error('Pipeline YAML serialization changed its contract');
  }
  return result;
}

/** Keep the existing non-throwing API. Use the strict serializer for editor writes. */
export function dumpYaml(data: unknown, options: DumpYamlOptions = {}): string {
  try {
    return serializePipelineYaml(reorderNodeKeys(data), options);
  } catch (caught) {
    const message = caught instanceof Error ? caught.message : String(caught);
    return `Error dumping YAML: ${message}`;
  }
}
