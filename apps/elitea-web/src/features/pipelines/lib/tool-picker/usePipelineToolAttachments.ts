import { useCallback, useEffect, useRef } from 'react';
import { flushSync } from 'react-dom';

import { getApplication } from '@/shared/api/generated/applications/applications';
import { getToolkit } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapBody } from '@/shared/api/unwrap';
import { buildErrorMessage } from '@/shared/lib/http-error';

import { useGetToolkitNameFromSchema } from '../flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../flow-editor/hooks/useToolkitTypeSchemas';
import { getAttachmentKey } from './attachmentDiff.helpers';
import {
  buildApplicationAttachment,
  buildToolkitAttachment,
  withApplicationVersion,
} from './attachmentTool.helpers';
import { isApplicationTool } from './pipelineToolUsage.helpers';
import { APPLICATION_TOOL_TYPE } from './toolPicker.constants';
import type {
  ApplicationListRow,
  GetToolkitNameFn,
  PickerToolEntry,
  ToolkitListRow,
} from './toolPicker.types';

const SWARM_TOOL_NAME = 'swarm';

export interface UsePipelineToolAttachmentsParams {
  readonly tools: readonly PickerToolEntry[];
  readonly onToolsChange: (nextTools: readonly PickerToolEntry[]) => void;
  readonly projectId?: string | undefined;
  readonly onError?: ((message: string) => void) | undefined;
}

export interface UsePipelineToolAttachmentsResult {
  readonly tools: readonly PickerToolEntry[];
  readonly addTool: (tool: PickerToolEntry) => PickerToolEntry;
  readonly addToolWithNode: (
    tool: PickerToolEntry,
    writeNode: (attached: PickerToolEntry) => void,
  ) => PickerToolEntry;
  readonly attachToolkit: (
    toolkit: ToolkitListRow | PickerToolEntry,
    options?: { readonly mcp?: boolean | undefined; readonly toolkitName?: string | undefined },
  ) => Promise<PickerToolEntry | null>;
  readonly prepareToolkitAttachment: (
    toolkit: ToolkitListRow | PickerToolEntry,
    options?: { readonly mcp?: boolean | undefined; readonly toolkitName?: string | undefined },
  ) => Promise<PickerToolEntry | null>;
  readonly prepareApplicationAttachment: (
    application: ApplicationListRow | PickerToolEntry,
  ) => Promise<PickerToolEntry | null>;
  readonly setApplicationVersion: (
    applicationId: number,
    versionDetails: { readonly id?: number | undefined; readonly icon_meta?: Readonly<Record<string, unknown>> | undefined },
  ) => void;
  readonly detach: (tool: PickerToolEntry) => void;
  readonly getAttachedApplication: (name: string) => PickerToolEntry | undefined;
  readonly getToolkitName: GetToolkitNameFn;
}

function formatError(error: unknown): string {
  const msg = buildErrorMessage(error);
  return typeof msg === 'string' && msg ? msg : 'Request failed';
}

/**
 * Attaches toolkits, MCPs, agents and pipelines picked in nodes. Edits the form state only:
 * Save applies the relations (`useApplyPipelineAttachments`) and Discard reverts them.
 */
export function usePipelineToolAttachments({
  tools,
  onToolsChange,
  projectId: projectIdOverride,
  onError,
}: UsePipelineToolAttachmentsParams): UsePipelineToolAttachmentsResult {
  const routeProjectId = useSelectedProjectId();
  const projectId = projectIdOverride ?? routeProjectId ?? '';
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId || undefined);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const latestToolsRef = useRef<readonly PickerToolEntry[]>(tools);
  useEffect(() => {
    latestToolsRef.current = tools;
  }, [tools]);

  const writeTools = useCallback(
    (nextTools: readonly PickerToolEntry[]) => {
      latestToolsRef.current = nextTools;
      onToolsChange(nextTools);
    },
    [onToolsChange],
  );

  const findAttached = useCallback(
    (key: string) => latestToolsRef.current.find((tool) => getAttachmentKey(tool) === key),
    [],
  );

  const addTool = useCallback(
    (tool: PickerToolEntry): PickerToolEntry => {
      const attached = findAttached(getAttachmentKey(tool));
      if (attached) return attached;
      writeTools([...latestToolsRef.current, tool]);
      return tool;
    },
    [findAttached, writeTools],
  );

  const addToolWithNode = useCallback(
    (tool: PickerToolEntry, writeNode: (attached: PickerToolEntry) => void): PickerToolEntry => {
      let attached = tool;
      flushSync(() => {
        attached = addTool(tool);
        writeNode(attached);
      });
      return attached;
    },
    [addTool],
  );

  const prepareToolkitAttachment = useCallback(
    async (
      toolkit: ToolkitListRow | PickerToolEntry,
      { mcp = false, toolkitName }: { readonly mcp?: boolean | undefined; readonly toolkitName?: string | undefined } = {},
    ): Promise<PickerToolEntry | null> => {
      if (!toolkit?.id) return null;
      const attached = findAttached(getAttachmentKey(toolkit));
      if (attached) return attached;

      let source: ToolkitListRow | PickerToolEntry = toolkit;
      if (!toolkit.settings) {
        try {
          const response = await getToolkit(projectId, Number(toolkit.id));
          const body = unwrapBody(response) as ToolkitListRow | PickerToolEntry | undefined;
          if (body) source = body;
        } catch (error: unknown) {
          onError?.(formatError(error));
          return null;
        }
      }

      return buildToolkitAttachment(source, {
        mcp,
        toolkitName: toolkitName || source.toolkit_name || getToolkitNameFromSchema(source),
      });
    },
    [findAttached, getToolkitNameFromSchema, onError, projectId],
  );

  const attachToolkit = useCallback(
    async (
      toolkit: ToolkitListRow | PickerToolEntry,
      options?: { readonly mcp?: boolean | undefined; readonly toolkitName?: string | undefined },
    ): Promise<PickerToolEntry | null> => {
      const tool = await prepareToolkitAttachment(toolkit, options);
      return tool ? addTool(tool) : null;
    },
    [addTool, prepareToolkitAttachment],
  );

  const prepareApplicationAttachment = useCallback(
    async (application: ApplicationListRow | PickerToolEntry): Promise<PickerToolEntry | null> => {
      if (!application?.id) return null;
      let details: PickerToolEntry | undefined;
      try {
        const response = await getApplication(projectId, Number(application.id));
        details = unwrapBody(response) as PickerToolEntry | undefined;
      } catch (error: unknown) {
        onError?.(formatError(error));
        return null;
      }
      if (!details) return null;
      const internalTools = details.version_details?.meta?.internal_tools ?? [];
      if (internalTools.includes(SWARM_TOOL_NAME)) {
        onError?.(
          `"${application.name ?? ''}" has Swarm Mode enabled and cannot be used in a pipeline. ` +
            'Swarm agents are only supported in direct chat.',
        );
        return null;
      }
      return buildApplicationAttachment(details);
    },
    [onError, projectId],
  );

  const setApplicationVersion = useCallback(
    (
      applicationId: number,
      versionDetails: { readonly id?: number | undefined; readonly icon_meta?: Readonly<Record<string, unknown>> | undefined },
    ) => {
      const key = getAttachmentKey({
        type: APPLICATION_TOOL_TYPE,
        settings: { application_id: applicationId },
      });
      const attached = findAttached(key);
      if (!attached || attached.settings?.application_version_id === versionDetails?.id) return;
      writeTools(
        latestToolsRef.current.map((tool) =>
          getAttachmentKey(tool) === key ? withApplicationVersion(tool, versionDetails) : tool,
        ),
      );
    },
    [findAttached, writeTools],
  );

  const detach = useCallback(
    (tool: PickerToolEntry) => {
      const key = getAttachmentKey(tool);
      writeTools(latestToolsRef.current.filter((item) => getAttachmentKey(item) !== key));
    },
    [writeTools],
  );

  const getAttachedApplication = useCallback(
    (name: string) => tools.find((tool) => isApplicationTool(tool) && tool.name === name),
    [tools],
  );

  return {
    tools,
    addTool,
    addToolWithNode,
    attachToolkit,
    prepareToolkitAttachment,
    prepareApplicationAttachment,
    setApplicationVersion,
    detach,
    getAttachedApplication,
    getToolkitName: getToolkitNameFromSchema,
  };
}
