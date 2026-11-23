import { useCallback, useContext, useEffect, useMemo, useState } from 'react';
import { dump, load } from 'js-yaml';

import { t } from '@/shared/i18n';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import {
  findRenameRejection,
  renameFlowEdge,
  renameFlowNode,
  renameYamlDocument,
} from '../nodes/BaseNode/NodeCardHeader.rename';

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

/**
 * Manages per-node YAML serialization, parsing, validation, and rename-safe writes
 * inside `NodeConfigPanel`'s YAML view.
 */
export function useNodeYamlEditor({
  nodeId,
  onRenamed,
  toolNames = [],
}: UseNodeYamlEditorArgs): UseNodeYamlEditorResult {
  const flowContext = useContext(FlowEditorContext);
  const yamlJsonObject = flowContext?.yamlJsonObject;

  const yamlNode = useMemo(
    () => yamlJsonObject?.nodes?.find((entry) => entry.id === nodeId),
    [nodeId, yamlJsonObject?.nodes],
  );
  const serialized = useMemo(() => serializeYamlNode(yamlNode), [yamlNode]);

  const [draft, setDraft] = useState(serialized);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setDraft(serialized);
    setError(null);
  }, [nodeId, serialized]);

  const onChangeCode = useCallback(
    (nextCode: string) => {
      setDraft(nextCode);
      if (!flowContext) return;

      let parsed: unknown;
      try {
        parsed = load(nextCode);
      } catch (err) {
        setError(err instanceof Error ? err.message : t('pipelines.nodeConfigPanel.yamlInvalid', 'Invalid YAML syntax.'));
        return;
      }

      if (!isPlainRecord(parsed)) {
        setError(t('pipelines.nodeConfigPanel.yamlSingleNodeError', 'The node YAML must describe exactly one node.'));
        return;
      }

      const nextId = typeof parsed['id'] === 'string' ? parsed['id'] : '';
      if (!nextId) {
        setError(t('pipelines.nodeConfigPanel.yamlMissingIdError', 'The node YAML must include a valid id.'));
        return;
      }

      const nextNode = parsed as unknown as YamlPipelineNode;
      if (nextId !== nodeId) {
        const rejection = findRenameRejection(nextId, nodeId, flowContext.yamlJsonObject, toolNames);
        if (rejection) {
          setError(rejection);
          return;
        }
        const renamedDoc = renameYamlDocument(flowContext.yamlJsonObject, nodeId, nextId);
        const updatedNodes = (renamedDoc.nodes ?? []).map((entry) => (entry.id === nextId ? nextNode : entry));
        flowContext.setYamlJsonObject({ ...renamedDoc, nodes: updatedNodes });
        flowContext.setFlowNodes((prevNodes: FlowNode[]) => prevNodes.map((n) => renameFlowNode(n, nodeId, nextId)));
        flowContext.setFlowEdges((prevEdges: FlowEdge[]) => prevEdges.map((e) => renameFlowEdge(e, nodeId, nextId)));
        setError(null);
        onRenamed?.(nextId);
        return;
      }

      const updatedNodes = (flowContext.yamlJsonObject.nodes ?? []).map((entry) =>
        entry.id === nodeId ? nextNode : entry,
      );
      flowContext.setYamlJsonObject({ ...flowContext.yamlJsonObject, nodes: updatedNodes });
      setError(null);
    },
    [flowContext, nodeId, onRenamed, toolNames],
  );

  return { code: draft, error, onChangeCode };
}
