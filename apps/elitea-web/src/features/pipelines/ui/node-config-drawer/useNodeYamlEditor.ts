import { useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import { dump, load } from 'js-yaml';

import { t } from '@/shared/i18n';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import {
  findRenameRejection,
  renameFlowEdge,
  renameFlowNode,
  renameYamlDocument,
} from '../nodes/BaseNode/NodeCardHeader.rename';

const { DECISION_NODE_ID_SUFFIX } = FlowEditorConstants;

/** Maximum byte size accepted by per-node YAML parsing (64 KiB). */
export const MAX_NODE_YAML_BYTES = 64 * 1024;

const YAML_ALIAS_OR_ANCHOR_RE = /(?:^|[\s,[\]{}:])(?:&|\*)[A-Za-z0-9_-]+/;
const encoder = new TextEncoder();

export interface UseNodeYamlEditorArgs {
  readonly nodeId: string;
  readonly onRenamed?: ((newId: string) => void) | undefined;
  readonly toolNames?: readonly string[] | undefined;
}

export interface UseNodeYamlEditorResult {
  readonly code: string;
  readonly error: string | null;
  readonly onChangeCode: (nextCode: string) => void;
}

function serializeYamlNode(node: YamlPipelineNode | undefined): string {
  if (!node) return '';
  try {
    return dump(node, { lineWidth: -1, noRefs: true });
  } catch {
    return '';
  }
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function resolveYamlNodeId(nodeId: string): string {
  return nodeId.endsWith(DECISION_NODE_ID_SUFFIX)
    ? nodeId.slice(0, -DECISION_NODE_ID_SUFFIX.length)
    : nodeId;
}

function parseSingleNodeYaml(
  nextCode: string,
  expectedType: string | undefined,
): { readonly node?: YamlPipelineNode; readonly error?: string } {
  if (encoder.encode(nextCode).length > MAX_NODE_YAML_BYTES) {
    return {
      error: t('pipelines.nodeConfigPanel.yamlTooLargeError', 'The node YAML exceeds the maximum allowed size.'),
    };
  }
  if (YAML_ALIAS_OR_ANCHOR_RE.test(nextCode)) {
    return {
      error: t(
        'pipelines.nodeConfigPanel.yamlAliasNotAllowedError',
        'YAML anchors and aliases are not supported in node YAML.',
      ),
    };
  }

  let parsed: unknown;
  try {
    parsed = load(nextCode);
  } catch (err) {
    return {
      error: err instanceof Error ? err.message : t('pipelines.nodeConfigPanel.yamlInvalid', 'Invalid YAML syntax.'),
    };
  }

  if (!isPlainRecord(parsed)) {
    return {
      error: t('pipelines.nodeConfigPanel.yamlSingleNodeError', 'The node YAML must describe exactly one node.'),
    };
  }

  const nextId = typeof parsed['id'] === 'string' ? parsed['id'] : '';
  if (!nextId) {
    return {
      error: t('pipelines.nodeConfigPanel.yamlMissingIdError', 'The node YAML must include a valid id.'),
    };
  }

  const nextType = typeof parsed['type'] === 'string' ? parsed['type'] : undefined;
  if (expectedType !== undefined && nextType !== expectedType) {
    return {
      error: t(
        'pipelines.nodeConfigPanel.yamlTypeImmutableError',
        'Changing the node type in YAML view is not supported.',
      ),
    };
  }

  return { node: parsed as unknown as YamlPipelineNode };
}

function applyParsedNodeUpdate(
  flowContext: FlowEditorContextValue,
  yamlNodeId: string,
  nextNode: YamlPipelineNode,
  toolNames: readonly string[],
  onRenamed: ((newId: string) => void) | undefined,
): string | null {
  const nextId = nextNode.id;
  if (nextId !== yamlNodeId) {
    const rejection = findRenameRejection(nextId, yamlNodeId, flowContext.yamlJsonObject, toolNames);
    if (rejection) return rejection;
    const renamedDoc = renameYamlDocument(flowContext.yamlJsonObject, yamlNodeId, nextId);
    const updatedNodes = (renamedDoc.nodes ?? []).map((entry) => (entry.id === nextId ? nextNode : entry));
    const stored = flowContext.setYamlJsonObject({ ...renamedDoc, nodes: updatedNodes });
    if (stored === false) return null;
    flowContext.setFlowNodes((prevNodes: FlowNode[]) => prevNodes.map((n) => renameFlowNode(n, yamlNodeId, nextId)));
    flowContext.setFlowEdges((prevEdges: FlowEdge[]) => prevEdges.map((e) => renameFlowEdge(e, yamlNodeId, nextId)));
    onRenamed?.(nextId);
    return null;
  }

  const updatedNodes = (flowContext.yamlJsonObject.nodes ?? []).map((entry) =>
    entry.id === yamlNodeId ? nextNode : entry,
  );
  flowContext.setYamlJsonObject({ ...flowContext.yamlJsonObject, nodes: updatedNodes });
  return null;
}

/**
 * Manages per-node YAML serialization, bounded parsing, validation, read-only gating,
 * and rename-safe writes inside `NodeConfigPanel`'s YAML view.
 */
export function useNodeYamlEditor({
  nodeId,
  onRenamed,
  toolNames = [],
}: UseNodeYamlEditorArgs): UseNodeYamlEditorResult {
  const flowContext = useContext(FlowEditorContext);
  const yamlJsonObject = flowContext?.yamlJsonObject;
  const yamlNodeId = resolveYamlNodeId(nodeId);
  const isSyntheticDecision = nodeId.endsWith(DECISION_NODE_ID_SUFFIX);

  const yamlNode = useMemo(
    () => yamlJsonObject?.nodes?.find((entry) => entry.id === yamlNodeId),
    [yamlNodeId, yamlJsonObject?.nodes],
  );
  const serialized = useMemo(() => serializeYamlNode(yamlNode), [yamlNode]);

  const [draft, setDraft] = useState(serialized);
  const [error, setError] = useState<string | null>(null);
  const lastCommittedSerializedRef = useRef<string | null>(null);

  useEffect(() => {
    if (lastCommittedSerializedRef.current !== null && serialized === lastCommittedSerializedRef.current) {
      lastCommittedSerializedRef.current = null;
      return;
    }
    setDraft(serialized);
    setError(null);
  }, [nodeId, serialized]);

  const onChangeCode = useCallback(
    (nextCode: string) => {
      if (!flowContext || flowContext.disabled || flowContext.isRunningPipeline || isSyntheticDecision) return;
      setDraft(nextCode);

      const result = parseSingleNodeYaml(nextCode, typeof yamlNode?.type === 'string' ? yamlNode.type : undefined);
      if (result.error || !result.node) {
        setError(result.error ?? t('pipelines.nodeConfigPanel.yamlInvalid', 'Invalid YAML syntax.'));
        return;
      }

      lastCommittedSerializedRef.current = serializeYamlNode(result.node);
      const writeError = applyParsedNodeUpdate(flowContext, yamlNodeId, result.node, toolNames, onRenamed);
      if (writeError) {
        lastCommittedSerializedRef.current = null;
        setError(writeError);
        return;
      }
      setError(null);
    },
    [flowContext, isSyntheticDecision, onRenamed, toolNames, yamlNode?.type, yamlNodeId],
  );

  return { code: draft, error, onChangeCode };
}
