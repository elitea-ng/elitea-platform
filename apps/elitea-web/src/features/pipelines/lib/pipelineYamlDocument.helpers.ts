import { loadAll } from 'js-yaml';

import { normalizePipelineNodeIdentifiers } from '@/shared/lib/pipelineNodeIdentifiers';

import { dumpYaml, serializePipelineYaml } from './dumpYaml.helpers';
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

/** Parsing changes the view and its declaration sequence, never the source text. Integer node ids are viewed as their decimal strings. */
export function parsePipelineYamlDocument(yamlCode: string): PipelineYamlDocumentSnapshot {
  const documents = loadAll(yamlCode);
  if (documents.length > 1) throw new Error('Expected one pipeline YAML document');
  const document: unknown = normalizePipelineNodeIdentifiers(documents[0] ?? {});
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

/**
 * Whether `nextCode` is nothing but the editor re-dumping a document whose
 * stored text spells integer node ids unquoted (`id: 1`).
 *
 * The flow editor holds those ids as strings, so `EditorPanel` re-dumping its
 * document on a Flow -> Yaml switch writes them quoted (`id: '1'`). To the
 * runtime that is the same pipeline, but the dirty check compares the parsed
 * values (1 vs "1") and would arm the unsaved-changes guard for a user who
 * changed nothing. The store therefore keeps the author's text for exactly
 * this case, and only this case:
 *  - the stored text really spells an identifier as a number,
 *  - the flow document is still that very document (no edit pending), and
 *  - `nextCode` is byte-for-byte the editor's own `dumpYaml` of it.
 * Anything a person types differs from the canonical dump (or from the
 * flow document) and is stored as usual.
 */
export function isIdentifierRespellingRedump(
  current: Pick<PipelineYamlDocumentSnapshot, 'yamlCode' | 'yamlJsonObject'>,
  nextCode: string,
): boolean {
  if (nextCode === current.yamlCode) return false;
  let raw: unknown;
  try {
    const documents = loadAll(current.yamlCode);
    if (documents.length !== 1) return false;
    raw = documents[0];
  } catch {
    return false;
  }
  const normalized = normalizePipelineNodeIdentifiers(raw);
  if (normalized === raw || typeof normalized !== 'object' || normalized === null) return false;
  const order = readPipelineStateOrder(current.yamlCode);
  if (pipelineValueFingerprint(normalized, order) !== pipelineValueFingerprint(current.yamlJsonObject, order)) return false;
  return nextCode === dumpYaml(current.yamlJsonObject);
}
