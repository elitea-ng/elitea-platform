import { loadAll } from 'js-yaml';

import { serializePipelineYaml } from './dumpYaml.helpers';
import {
  pipelineValueFingerprint,
  readPipelineStateOrder,
  reconcilePipelineStateOrder,
  renamePipelineStateOrder,
  validatePipelineStateOrder,
} from './pipelineYamlState.helpers';

export interface PipelineYamlEditOptions {
  readonly stateRename?: { readonly from: string; readonly to: string };
  readonly stateKeyOrder?: readonly string[];
}

export interface PipelineYamlDocumentSnapshot {
  readonly yamlCode: string;
  readonly yamlJsonObject: Readonly<Record<string, unknown>>;
  readonly stateKeyOrder: readonly string[];
}

/** Parsing changes the view and its declaration sequence, never the source text. */
export function parsePipelineYamlDocument(yamlCode: string): PipelineYamlDocumentSnapshot {
  const documents = loadAll(yamlCode);
  if (documents.length > 1) throw new Error('Expected one pipeline YAML document');
  const document: unknown = documents[0] ?? {};
  if (typeof document !== 'object' || document === null || Array.isArray(document)) {
    throw new Error('Expected a pipeline YAML mapping');
  }
  const stateKeyOrder = readPipelineStateOrder(yamlCode);
  validatePipelineStateOrder(document, stateKeyOrder);
  return { yamlCode, yamlJsonObject: document as Readonly<Record<string, unknown>>, stateKeyOrder };
}

export function pipelineYamlDocumentsEqual(
  left: Pick<PipelineYamlDocumentSnapshot, 'yamlJsonObject' | 'stateKeyOrder'>,
  right: Pick<PipelineYamlDocumentSnapshot, 'yamlJsonObject' | 'stateKeyOrder'>,
): boolean {
  return (
    pipelineValueFingerprint(left.yamlJsonObject, left.stateKeyOrder) ===
    pipelineValueFingerprint(right.yamlJsonObject, right.stateKeyOrder)
  );
}

/** Prepare and verify the entire edit before a caller publishes any field. */
export function preparePipelineYamlEdit(
  current: PipelineYamlDocumentSnapshot,
  next: Readonly<Record<string, unknown>>,
  options: PipelineYamlEditOptions = {},
): PipelineYamlDocumentSnapshot {
  const original = parsePipelineYamlDocument(current.yamlCode);
  if (!pipelineYamlDocumentsEqual(current, original))
    throw new Error('Parse current YAML before editing its flow document');
  let order = original.stateKeyOrder;
  if (options.stateRename) {
    const { from, to } = options.stateRename;
    const state = next['state'];
    if (
      state === null ||
      typeof state !== 'object' ||
      (from !== to && Object.hasOwn(state, from)) ||
      !Object.hasOwn(state, to)
    )
      throw new Error('Invalid pipeline state rename candidate');
    order = renamePipelineStateOrder(order, from, to);
  }
  const stateKeyOrder = options.stateKeyOrder ?? reconcilePipelineStateOrder(next, order);
  validatePipelineStateOrder(next, stateKeyOrder);
  const candidate = { yamlJsonObject: next, stateKeyOrder };
  if (pipelineYamlDocumentsEqual(original, candidate)) return original;
  const yamlCode = serializePipelineYaml(next, { originalYaml: current.yamlCode, stateKeyOrder });
  return { yamlCode, yamlJsonObject: next, stateKeyOrder: [...stateKeyOrder] };
}
