import { useEffect, useMemo } from 'react';
import { load as loadYaml } from 'js-yaml';

import { useGetToolkitNameFromSchema } from '../flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../flow-editor/hooks/useToolkitTypeSchemas';
import { reconcileAttachments } from './attachmentReconcile.helpers';
import type { PickerToolEntry, PipelineYamlDocument } from './toolPicker.types';

function parseYamlSafe(yamlCode: string | undefined): PipelineYamlDocument {
  if (!yamlCode) return {};
  try {
    const parsed: unknown = loadYaml(yamlCode);
    return parsed && typeof parsed === 'object' ? (parsed as PipelineYamlDocument) : {};
  } catch {
    return null;
  }
}

export interface UseReconcilePipelineAttachmentsParams {
  readonly tools: readonly PickerToolEntry[] | undefined;
  readonly initialTools: readonly PickerToolEntry[] | undefined;
  readonly yamlJsonObject: PipelineYamlDocument;
  readonly initialInstructions?: string | undefined;
  readonly initYamlCode?: string | undefined;
  readonly onToolsChange: (nextTools: readonly PickerToolEntry[]) => void;
  readonly projectId?: string | undefined;
  readonly enabled?: boolean | undefined;
}

/**
 * Keeps `version_details.tools` in line with the nodes after every YAML change (node delete, tool change,
 * YAML edit). Call once per pipeline editor.
 */
export function useReconcilePipelineAttachments({
  tools,
  initialTools,
  yamlJsonObject,
  initialInstructions = '',
  initYamlCode,
  onToolsChange,
  projectId: projectIdOverride,
  enabled = true,
}: UseReconcilePipelineAttachmentsParams): void {
  const routeProjectId = useSelectedProjectId();
  const projectId = projectIdOverride ?? routeProjectId;
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const initialYamlJsonObject = useMemo(() => parseYamlSafe(initialInstructions), [initialInstructions]);
  const isCanvasOfThisForm = initYamlCode === undefined || initYamlCode === initialInstructions;

  useEffect(() => {
    if (!enabled || !isCanvasOfThisForm || !initialYamlJsonObject || !Array.isArray(tools)) return;
    const nextTools = reconcileAttachments({
      tools,
      initialTools,
      yamlJsonObject,
      initialYamlJsonObject,
      getToolkitName: getToolkitNameFromSchema,
    });
    if (nextTools !== tools) {
      onToolsChange(nextTools);
    }
  }, [
    enabled,
    getToolkitNameFromSchema,
    initialTools,
    initialYamlJsonObject,
    isCanvasOfThisForm,
    onToolsChange,
    tools,
    yamlJsonObject,
  ]);
}
