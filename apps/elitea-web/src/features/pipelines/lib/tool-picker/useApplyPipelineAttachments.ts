import { useCallback, useState } from 'react';

import { updateApplicationRelation } from '@/shared/api/generated/applications/applications';
import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';
import { buildErrorMessage } from '@/shared/lib/http-error';

import { useSelectedProjectId } from '../flow-editor/hooks/useSelectedProjectId';
import { diffAttachments, getAttachmentKey } from './attachmentDiff.helpers';
import { isApplicationTool } from './pipelineToolUsage.helpers';
import { IDEMPOTENT_RELATION_ERRORS } from './toolPicker.constants';
import type { PickerToolEntry } from './toolPicker.types';

const MISSING_TOOLKIT_ERROR = 'No such toolkit';

function readNestedErrorString(container: unknown): string {
  if (typeof container === 'string') return container;
  if (!container || typeof container !== 'object') return '';
  const err = (container as Record<string, unknown>)['error'];
  return typeof err === 'string' ? err : '';
}

function getErrorText(error: unknown): string {
  if (typeof error === 'string') return error;
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const bodyErr = readNestedErrorString(error.failure.body);
    if (bodyErr) return bodyErr;
  }
  if (error && typeof error === 'object') {
    const rec = error as Record<string, unknown>;
    const nested = readNestedErrorString(rec['data']) || readNestedErrorString(rec['body']);
    if (nested) return nested;
    if (typeof rec['message'] === 'string') return rec['message'];
  }
  return '';
}

function isIdempotentError(error: unknown, isRemoval: boolean): boolean {
  const text = getErrorText(error);
  if (!text) return false;
  return (
    IDEMPOTENT_RELATION_ERRORS.some((message) => text.includes(message)) ||
    (isRemoval && text.includes(MISSING_TOOLKIT_ERROR))
  );
}

function getSelectedTools(tool: PickerToolEntry): readonly string[] | undefined {
  const selected = tool.settings?.selected_tools;
  if (!selected || selected.length === 0) return undefined;
  const names: string[] = [];
  for (const item of selected) {
    const name = typeof item === 'string' ? item : item?.name;
    if (typeof name === 'string' && name.length > 0) names.push(name);
  }
  return names.length > 0 ? names : undefined;
}

function formatRelationError(tool: PickerToolEntry, error: unknown, action: 'add' | 'switch' | 'remove'): string {
  const built = buildErrorMessage(error);
  const builtText = typeof built === 'string' && built ? built : 'Request failed';
  const detail = getErrorText(error) || builtText;
  if (isApplicationTool(tool)) {
    const entityLabel = tool.agent_type === 'pipeline' ? 'pipeline' : 'agent';
    const verb = action === 'remove' ? 'remove' : action === 'switch' ? 'switch version of' : 'add';
    return `Failed to ${verb} ${entityLabel} "${tool.name ?? ''}": ${detail}`;
  }
  const verb = action === 'remove' ? 'remove' : 'save';
  return `Failed to ${verb} toolkit "${tool.toolkit_name || tool.name || ''}": ${detail}`;
}

async function patchToolkitRelation(params: {
  readonly projectId: string;
  readonly applicationId: number;
  readonly versionId: number;
  readonly tool: PickerToolEntry;
  readonly hasRelation: boolean;
}): Promise<void> {
  const toolkitId = params.tool.tool_id ?? params.tool.id;
  const selectedTools = params.hasRelation ? getSelectedTools(params.tool) : undefined;
  await eliteaFetch(
    `/elitea_core/tool/prompt_lib/${encodeURIComponent(params.projectId)}/${encodeURIComponent(String(toolkitId ?? ''))}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        entity_version_id: params.versionId,
        entity_id: params.applicationId,
        entity_type: 'agent',
        has_relation: params.hasRelation,
        ...(selectedTools === undefined ? {} : { selected_tools: selectedTools }),
      }),
    },
  );
}

async function patchApplicationRelation(params: {
  readonly projectId: string;
  readonly applicationId: number;
  readonly versionId: number;
  readonly tool: PickerToolEntry;
  readonly hasRelation: boolean;
}): Promise<{ readonly tool_id?: number | undefined } | null> {
  const response = await updateApplicationRelation(
    params.projectId,
    Number(params.tool.settings?.application_id ?? 0),
    Number(params.tool.settings?.application_version_id ?? 0),
    {
      application_id: params.applicationId,
      version_id: params.versionId,
      has_relation: params.hasRelation,
    },
  );
  const body = unwrapBody(response);
  if (!body || typeof body !== 'object') return null;
  const toolId = (body as Record<string, unknown>)['tool_id'];
  return typeof toolId === 'number' ? { tool_id: toolId } : {};
}

export interface ApplyAttachmentsInput {
  readonly applicationId: number;
  readonly versionId: number;
  readonly initialTools?: readonly PickerToolEntry[] | undefined;
  readonly tools?: readonly PickerToolEntry[] | undefined;
}

export interface UseApplyPipelineAttachmentsParams {
  readonly projectId?: string | undefined;
  readonly onError?: ((message: string) => void) | undefined;
}

export interface UseApplyPipelineAttachmentsResult {
  readonly applyAttachments: (
    input: ApplyAttachmentsInput,
  ) => Promise<false | { readonly tools: readonly PickerToolEntry[] }>;
  readonly isApplyingAttachments: boolean;
}

/**
 * Applies the attachments picked in nodes to the saved version, before the version itself is saved.
 * Each request is idempotent, so a failed Save can simply be retried.
 */
export function useApplyPipelineAttachments({
  projectId: projectIdOverride,
  onError,
}: UseApplyPipelineAttachmentsParams = {}): UseApplyPipelineAttachmentsResult {
  const routeProjectId = useSelectedProjectId();
  const projectId = projectIdOverride ?? routeProjectId ?? '';
  const [isApplyingAttachments, setIsApplyingAttachments] = useState(false);

  const applyAttachments = useCallback(
    async ({
      applicationId,
      versionId,
      initialTools = [],
      tools = [],
    }: ApplyAttachmentsInput): Promise<false | { readonly tools: readonly PickerToolEntry[] }> => {
      const { added, removed, selectedToolsChanged, versionChanged, hasChanges } = diffAttachments(
        initialTools,
        tools,
      );
      if (!hasChanges) return { tools };

      const toolIdsByKey: Record<string, number> = {};
      const run = async <T,>(
        tool: PickerToolEntry,
        request: () => Promise<T>,
        action: 'add' | 'switch' | 'remove',
      ): Promise<T | null> => {
        try {
          return await request();
        } catch (error: unknown) {
          if (isIdempotentError(error, action === 'remove')) return null;
          onError?.(formatRelationError(tool, error, action));
          throw error;
        }
      };

      setIsApplyingAttachments(true);
      try {
        for (const tool of added) {
          if (isApplicationTool(tool)) {
            const result = await run(
              tool,
              () => patchApplicationRelation({ projectId, applicationId, versionId, tool, hasRelation: true }),
              'add',
            );
            if (result?.tool_id) toolIdsByKey[getAttachmentKey(tool)] = result.tool_id;
          } else {
            await run(
              tool,
              () => patchToolkitRelation({ projectId, applicationId, versionId, tool, hasRelation: true }),
              'add',
            );
          }
        }
        for (const { tool } of versionChanged) {
          await run(
            tool,
            () => patchApplicationRelation({ projectId, applicationId, versionId, tool, hasRelation: true }),
            'switch',
          );
        }
        for (const tool of selectedToolsChanged) {
          await run(
            tool,
            () => patchToolkitRelation({ projectId, applicationId, versionId, tool, hasRelation: true }),
            'add',
          );
        }
        for (const tool of removed) {
          await run<void>(
            tool,
            async () => {
              if (isApplicationTool(tool)) {
                await patchApplicationRelation({ projectId, applicationId, versionId, tool, hasRelation: false });
              } else {
                await patchToolkitRelation({ projectId, applicationId, versionId, tool, hasRelation: false });
              }
            },
            'remove',
          );
        }
      } catch {
        return false;
      } finally {
        setIsApplyingAttachments(false);
      }

      return {
        tools: tools.map((tool) => {
          const toolId = toolIdsByKey[getAttachmentKey(tool)];
          return toolId && !tool.id ? { ...tool, id: toolId } : tool;
        }),
      };
    },
    [onError, projectId],
  );

  return { applyAttachments, isApplyingAttachments };
}
